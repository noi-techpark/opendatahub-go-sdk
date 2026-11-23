// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/dc"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/reftable"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Reporter is the production Observer.
type Reporter struct {
	cfg    Config
	bridge *rdb.RDBridge
	now    func() time.Time

	mu    sync.Mutex
	ctx   context.Context // from Start; Observe has none of its own
	items map[string]*item
}

// item is the in-memory state of one key.
type item struct {
	rec Record
	// hash covers rec without FirstSeen/Changed.
	hash string
	// written is the hash last acknowledged by the raw writer. The item is
	// pending exactly while it differs from hash, so a change that lands during
	// a Flush's HTTP calls is still pending afterwards.
	written string
	// lastTS is the timestamp of the last write of this key.
	lastTS time.Time
	// seen is whether the key was observed since the previous Complete.
	seen   bool
	misses int
}

func (it *item) pending() bool { return it.hash != it.written }

// New builds a Reporter. bridge reads the table back at Start.
func New(bridge *rdb.RDBridge, cfg Config) *Reporter {
	if cfg.Name == "" {
		cfg.Name = cfg.Provider
	}
	return &Reporter{
		cfg:    cfg,
		bridge: bridge,
		now:    func() time.Time { return time.Now().UTC() },
		items:  map[string]*item{},
	}
}

var (
	newTotalOnce sync.Once
	newTotal     otelmetric.Int64UpDownCounter
)

// newTotalCounter is built lazily: telemetry is configured in main, after
// package initialisation. tel offers no monotonic counter, so this is an
// up-down counter that is only ever incremented.
func newTotalCounter() otelmetric.Int64UpDownCounter {
	newTotalOnce.Do(func() {
		c, err := tel.MeterInt64UpDownCounter(tel.Metric{
			Name:        "discovery_new_total",
			Unit:        "{count}",
			Description: "Entities seen for the first time by a discovery reporter.",
		})
		if err == nil {
			newTotal = c
		}
	})
	return newTotal
}

func splitProvider(p string) (db, collection string, err error) {
	db, collection, ok := strings.Cut(p, "/")
	if !ok || db == "" || collection == "" || strings.Contains(collection, "/") {
		return "", "", fmt.Errorf("discovery provider must be '<db>/<collection>', got %q", p)
	}
	return db, collection, nil
}

func (r *Reporter) validate() (db, collection string, err error) {
	db, collection, err = splitProvider(r.cfg.Provider)
	if err != nil {
		return "", "", err
	}
	if r.cfg.RawWriterURL == "" {
		return "", "", errors.New("discovery RawWriterURL is empty")
	}
	if r.cfg.GoneAfter < 0 {
		return "", "", fmt.Errorf("discovery GoneAfter must not be negative, got %d", r.cfg.GoneAfter)
	}
	if r.bridge == nil {
		return "", "", errors.New("discovery needs a raw data bridge to read the existing table")
	}
	return db, collection, nil
}

// Start reads the existing table so a restart neither rewrites unchanged
// entries nor reports known keys as new.
//
// A collection that does not exist yet is a cold start, not an error: unlike
// an enrichment table nothing is lost by beginning empty, and the first write
// creates it.
func (r *Reporter) Start(ctx context.Context) error {
	db, collection, err := r.validate()
	if err != nil {
		return err
	}

	tbl := reftable.New[Record](r.bridge, reftable.Config{
		Name:       r.cfg.Name,
		DB:         db,
		Collection: collection,
		Key:        KeyField,
		Schema:     Schema,
		Decode:     reftable.DecodeText,
	})
	set := reftable.NewSet(tbl)
	err = set.Start(ctx)
	_ = set.Close()
	known := map[string]Record{}
	switch {
	case err == nil:
		known = tbl.All()
	case errors.Is(err, rdb.ErrCollectionNotFound):
		logger.Get(ctx).Info("discovery table does not exist yet, starting empty",
			"reporter", r.cfg.Name, "provider", r.cfg.Provider)
	default:
		return fmt.Errorf("read discovery table %s: %w", r.cfg.Provider, err)
	}

	items := make(map[string]*item, len(known))
	for key, rec := range known {
		canon, hash, err := canonical(rec)
		if err != nil {
			logger.Get(ctx).Warn("skipping unreadable discovery record",
				"reporter", r.cfg.Name, "key", key, "err", err)
			continue
		}
		canon.FirstSeen, canon.Changed = rec.FirstSeen, rec.Changed
		items[key] = &item{rec: canon, hash: hash, written: hash, lastTS: rec.Changed}
	}

	r.mu.Lock()
	r.ctx = ctx
	r.items = items
	r.mu.Unlock()

	logger.Get(ctx).Info("discovery reporter started",
		"reporter", r.cfg.Name, "provider", r.cfg.Provider, "known", len(items))
	return nil
}

// canonical returns rec as it round-trips through JSON, with FirstSeen and
// Changed cleared, and the hash of that form. Hashing the JSON makes map order
// irrelevant and gives a record built from an Entry the same hash as the one
// read back from the table. The copy shares no maps with the caller.
func canonical(rec Record) (Record, string, error) {
	rec.FirstSeen, rec.Changed = time.Time{}, time.Time{}
	b, err := json.Marshal(rec)
	if err != nil {
		return Record{}, "", err
	}
	// UseNumber, as reftable decodes: a large integer in Data must survive
	// exactly, and the held form then equals the one read back from the table.
	var out Record
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		return Record{}, "", err
	}
	sum := sha256.Sum256(b)
	return out, hex.EncodeToString(sum[:]), nil
}

// Observe records the entity for the next Flush. Safe for concurrent use.
func (r *Reporter) Observe(key string, e Entry) {
	r.mu.Lock()
	ctx := r.ctx
	r.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	log := logger.Get(ctx)

	if key == "" || (e.Status != StatusPublished && e.Status != StatusWithheld) {
		log.Error("discovery entry ignored: needs a key and a status of published or withheld",
			"reporter", r.cfg.Name, "key", key, "status", e.Status)
		return
	}
	rec, hash, err := canonical(Record{
		Status: e.Status, Reason: e.Reason, Name: e.Name, Parent: e.Parent,
		Lat: e.Lat, Lon: e.Lon, Data: e.Data, Seed: e.Seed,
	})
	if err != nil {
		log.Error("discovery entry ignored: not JSON-encodable",
			"reporter", r.cfg.Name, "key", key, "err", err)
		return
	}

	now := r.now()
	r.mu.Lock()
	it, known := r.items[key]
	switch {
	case !known:
		rec.FirstSeen, rec.Changed = now, now
		it = &item{rec: rec, hash: hash}
		r.items[key] = it
	case it.hash != hash:
		rec.FirstSeen, rec.Changed = it.rec.FirstSeen, now
		it.rec, it.hash = rec, hash
	}
	it.seen, it.misses = true, 0
	created := it.rec
	r.mu.Unlock()

	if known {
		return
	}
	log.Warn("discovered a new entity",
		"reporter", r.cfg.Name, "key", key, "status", string(created.Status), "reason", created.Reason)
	if c := newTotalCounter(); c != nil {
		c.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("reporter", r.cfg.Name)))
	}
	r.notifyNew(ctx, key, created)
}

// notifyNew guards the consumer's callback so a panic in it cannot take down
// the message handler that called Observe.
func (r *Reporter) notifyNew(ctx context.Context, key string, rec Record) {
	if r.cfg.OnNew == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			logger.Get(ctx).Error("discovery OnNew panicked",
				"reporter", r.cfg.Name, "key", key, "panic", p)
		}
	}()
	r.cfg.OnNew(ctx, key, rec)
}

type write struct {
	key  string
	rec  Record
	hash string
	last time.Time
}

// Flush writes every entry whose content changed since it was last written. A
// failed write stays pending for the next Flush.
func (r *Reporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	var batch []write
	for key, it := range r.items {
		if it.pending() {
			batch = append(batch, write{key: key, rec: it.rec, hash: it.hash, last: it.lastTS})
		}
	}
	r.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	sort.Slice(batch, func(i, j int) bool { return batch[i].key < batch[j].key })

	cctx, col := dc.NewCollection(ctx, r.cfg.RawWriterURL, r.cfg.Provider)
	// A recording ctx is the caller's own span; End would only stamp it Ok.
	if !trace.SpanFromContext(ctx).IsRecording() {
		defer col.End(cctx)
	}

	var errs []error
	for _, w := range batch {
		// The writer's path timestamp has second resolution and compaction picks
		// the newest per key, so two writes of a key must not share one.
		ts := r.now().Truncate(time.Second)
		if !ts.After(w.last) {
			ts = w.last.Truncate(time.Second).Add(time.Second)
		}
		err := col.Publish(cctx, &rdb.RawAny{
			Rawdata:   w.rec,
			Timestamp: ts,
			Meta:      map[string]string{KeyField: w.key, "schema": Schema},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("discovery %s key %q: %w", r.cfg.Provider, w.key, err))
			continue
		}
		r.mu.Lock()
		if it := r.items[w.key]; it != nil {
			it.written, it.lastTS = w.hash, ts
		}
		r.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Complete declares that the entries observed since the previous Complete are
// the source's full set. Only full-snapshot sources call it.
func (r *Reporter) Complete() {
	now := r.now()
	var gone []string

	r.mu.Lock()
	for key, it := range r.items {
		seen := it.seen
		it.seen = false
		if seen || r.cfg.GoneAfter <= 0 || it.rec.Status == StatusGone {
			continue
		}
		it.misses++
		if it.misses < r.cfg.GoneAfter {
			continue
		}
		it.rec.Status = StatusGone
		it.rec.Changed = now
		if _, h, err := canonical(it.rec); err == nil {
			it.hash = h
			gone = append(gone, key)
		}
	}
	ctx := r.ctx
	r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	sort.Strings(gone)
	for _, key := range gone {
		logger.Get(ctx).Info("entity is gone from the source",
			"reporter", r.cfg.Name, "key", key, "after", r.cfg.GoneAfter)
	}
}
