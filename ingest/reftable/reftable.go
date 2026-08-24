// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

// Package reftable materialises operator-authored reference data inside a
// transformer.
//
// A transformer consumes one stream of records. Some transformations also need
// slowly-changing lookup data — station enrichment, code tables, per-provider
// configuration — which historically was baked into the image as a CSV or read
// from a spreadsheet at runtime. This package replaces that with a table kept
// current from the raw data lake:
//
//   - bootstrap reads the compacted view of a collection (the newest document
//     per key) and builds an in-memory map;
//   - a periodic sweep asks for everything written since the last high-water
//     mark, which is what keeps the table correct;
//   - an optional queue subscription applies single changes as they happen,
//     which only affects latency.
//
// Reference data is configuration, not a second record source: the transformer
// still consumes exactly one data queue, and a table that is briefly stale
// degrades the output rather than corrupting it.
package reftable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/urn"
	"github.com/noi-techpark/opendatahub-go-sdk/qmill"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
)

// Config describes one reference table.
type Config struct {
	// Name identifies the table in logs and metrics. Defaults to DB.Collection.
	Name string

	// DB, Collection and Key address the compacted view in the raw data lake.
	// Key names a field inside the stored document's `meta` sub-document — not
	// a path, and not a field at the document root. A publisher sets it by
	// sending an X-OpenDataHub-<Key> header, which is the only way to put a
	// groupable field on a document.
	DB         string
	Collection string
	Key        string

	// Schema, when set, is compared against the document's `meta.schema` field.
	Schema string

	// Decode reads the stored `rawdata` value. Defaults to DecodeText, which is
	// what raw-writer-2 stores for textual payloads; use DecodeBase64 for the
	// legacy rest-push encoding, or DecodeJSON when the payload was stored as an
	// object.
	Decode Decoder

	// OnChange, when set, is called after a refresh that applied at least one
	// record — never for a no-op sweep. It is how a consumer reacts to an
	// operator edit without polling the table.
	//
	// It runs on the refresh goroutine with no lock held, so it may read the
	// table freely. Keep it short, or hand off: a slow callback delays the next
	// refresh. It is not called during the initial bootstrap, since nothing has
	// changed from the consumer's point of view yet.
	OnChange func(ctx context.Context, applied int)

	// Sweep is how often the table asks for everything written since its
	// high-water mark. This is the mechanism that guarantees correctness;
	// the queue only reduces latency. Defaults to DefaultSweep.
	Sweep time.Duration

	// PageLimit caps documents per request during bootstrap. Defaults to
	// DefaultPageLimit.
	PageLimit int

	// MQ_* enable live change notifications. Leave MQ_URI empty to run
	// sweep-only, which is a supported configuration rather than a degraded one.
	MQ_URI      string
	MQ_CLIENT   string
	MQ_EXCHANGE string
	MQ_QUEUE    string
	MQ_KEY      string
}

const (
	DefaultSweep     = 5 * time.Minute
	DefaultPageLimit = 1000
	// maxBootstrapPages bounds the bootstrap walk so a mis-specified key that
	// never advances the cursor fails loudly instead of looping forever.
	maxBootstrapPages = 10000
)

var ErrNotReady = errors.New("reference table not bootstrapped")

// Table is a keyed, in-memory view of a reference collection. Reads are safe
// from the message handler while refreshes run.
type Table[T any] struct {
	cfg    Config
	bridge *rdb.RDBridge

	mu     sync.RWMutex
	data   map[string]T
	stamps map[string]time.Time // per key: timestamp of the document applied
	high   time.Time            // newest write seen, drives the sweep
	ready  bool

	readyCh   chan struct{}
	readyOnce sync.Once

	sub *qmill.QMill
}

// New creates a table. Nothing is fetched until Start.
func New[T any](bridge *rdb.RDBridge, cfg Config) *Table[T] {
	if cfg.Name == "" {
		cfg.Name = cfg.DB + "." + cfg.Collection
	}
	if cfg.Sweep <= 0 {
		cfg.Sweep = DefaultSweep
	}
	if cfg.PageLimit <= 0 {
		cfg.PageLimit = DefaultPageLimit
	}
	if cfg.Decode == nil {
		cfg.Decode = DecodeText
	}
	return &Table[T]{
		cfg:     cfg,
		bridge:  bridge,
		data:    map[string]T{},
		stamps:  map[string]time.Time{},
		readyCh: make(chan struct{}),
	}
}

func (t *Table[T]) Name() string { return t.cfg.Name }

// Get returns the current value for a key.
func (t *Table[T]) Get(key string) (T, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.data[key]
	return v, ok
}

// All returns a copy of the table. Callers may hold and mutate it freely.
func (t *Table[T]) All() map[string]T {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]T, len(t.data))
	for k, v := range t.data {
		out[k] = v
	}
	return out
}

func (t *Table[T]) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.data)
}

// HighWater is the newest write the table has applied.
func (t *Table[T]) HighWater() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.high
}

// Ready is closed once the first bootstrap has succeeded.
func (t *Table[T]) Ready() <-chan struct{} { return t.readyCh }

// bootstrap pages through the whole compacted view.
//
// Since is deliberately not used here: a key written once, long ago, still has
// a current value, so an incremental query cannot produce a complete table.
func (t *Table[T]) bootstrap(ctx context.Context) error {
	log := logger.Get(ctx)

	cursor := ""
	pages, applied := 0, 0

	for {
		page, err := t.bridge.GetLatest(ctx, rdb.LatestQuery{
			DB:         t.cfg.DB,
			Collection: t.cfg.Collection,
			Field:      t.cfg.Key,
			Cursor:     cursor,
			Limit:      t.cfg.PageLimit,
		})
		if err != nil {
			return fmt.Errorf("bootstrap %s: %w", t.cfg.Name, err)
		}

		applied += t.applyAll(ctx, page.Items)
		t.advance(page.HighWater)

		pages++
		if page.Next == "" {
			break
		}
		if page.Next == cursor {
			return fmt.Errorf("bootstrap %s: cursor did not advance past %q, check that meta.%s is a real field on the stored documents",
				t.cfg.Name, cursor, t.cfg.Key)
		}
		if pages >= maxBootstrapPages {
			return fmt.Errorf("bootstrap %s: exceeded %d pages", t.cfg.Name, maxBootstrapPages)
		}
		cursor = page.Next
	}

	t.mu.Lock()
	t.ready = true
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.readyCh) })

	log.Info("reference table bootstrapped",
		"table", t.cfg.Name, "keys", t.Len(), "applied", applied, "pages", pages)
	return nil
}

// sweep applies everything written since the high-water mark. This is what
// makes a missed notification a latency problem rather than a correctness one.
func (t *Table[T]) sweep(ctx context.Context) error {
	since := t.HighWater()
	if since.IsZero() {
		return t.bootstrap(ctx)
	}

	cursor := ""
	for {
		page, err := t.bridge.GetLatest(ctx, rdb.LatestQuery{
			DB:         t.cfg.DB,
			Collection: t.cfg.Collection,
			Field:      t.cfg.Key,
			Since:      &since,
			Cursor:     cursor,
			Limit:      t.cfg.PageLimit,
		})
		if err != nil {
			return fmt.Errorf("sweep %s: %w", t.cfg.Name, err)
		}

		if n := t.applyAll(ctx, page.Items); n > 0 {
			logger.Get(ctx).Info("reference table updated",
				"table", t.cfg.Name, "applied", n, "keys", t.Len())
			t.notify(ctx, n)
		}
		t.advance(page.HighWater)

		if page.Next == "" || page.Next == cursor {
			return nil
		}
		cursor = page.Next
	}
}

func (t *Table[T]) applyAll(ctx context.Context, items []json.RawMessage) int {
	n := 0
	for _, it := range items {
		r, err := parseRecord[T](it, t.cfg.Key, t.cfg.Decode)
		if err != nil {
			// One malformed record must not deny the consumer every other key.
			logger.Get(ctx).Warn("skipping reference document",
				"table", t.cfg.Name, "err", err)
			continue
		}
		if t.apply(ctx, r) {
			n++
		}
	}
	return n
}

// apply installs one record, refusing anything it cannot trust.
func (t *Table[T]) apply(ctx context.Context, r record[T]) bool {
	log := logger.Get(ctx)

	// A table that declares an expected schema takes it seriously: a record
	// stamped with another version, or with none at all, is refused. The
	// consumer's rules were written against one shape, and an unstamped record
	// is not evidence of anything — accepting it would mean guessing from the
	// data, which is exactly what the version exists to avoid.
	if t.cfg.Schema != "" && r.Schema != t.cfg.Schema {
		got := r.Schema
		if got == "" {
			got = "<unstamped>"
		}
		log.Warn("reference document with unexpected schema, skipped",
			"table", t.cfg.Name, "key", r.Key, "got", got, "want", t.cfg.Schema)
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Guard against out-of-order live notifications. Bootstrap and sweep are
	// already ordered by the bridge, so this only ever bites on the queue path.
	if prev, ok := t.stamps[r.Key]; ok && r.Timestamp.Before(prev) {
		log.Debug("ignoring older reference document",
			"table", t.cfg.Name, "key", r.Key, "got", r.Timestamp, "have", prev)
		return false
	}

	switch r.Op {
	case "", OpUpsert:
		t.data[r.Key] = r.Data
	case OpDelete:
		delete(t.data, r.Key)
	default:
		log.Warn("reference document with unknown op, skipped",
			"table", t.cfg.Name, "key", r.Key, "op", r.Op)
		return false
	}
	t.stamps[r.Key] = r.Timestamp
	return true
}

func (t *Table[T]) advance(hw *time.Time) {
	if hw == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if hw.After(t.high) {
		t.high = *hw
	}
}

// subscribe binds the change queue, if one is configured.
func (t *Table[T]) subscribe(ctx context.Context) error {
	if t.cfg.MQ_URI == "" {
		return nil
	}
	sub, err := qmill.NewSubscriberQmill(ctx, t.cfg.MQ_URI, t.cfg.MQ_CLIENT,
		qmill.WithQueue(t.cfg.MQ_QUEUE, true),
		qmill.WithBind(t.cfg.MQ_EXCHANGE, t.cfg.MQ_KEY),
		qmill.WithNoRequeueOnNack(true),
	)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", t.cfg.Name, err)
	}
	t.sub = sub
	return nil
}

// run consumes notifications and sweeps on a ticker until ctx is cancelled.
func (t *Table[T]) run(ctx context.Context) {
	log := logger.Get(ctx)
	ticker := time.NewTicker(t.cfg.Sweep)
	defer ticker.Stop()

	var notifications <-chan *message.Message
	if t.sub != nil {
		notifications = t.sub.Sub()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			if err := t.sweep(ctx); err != nil {
				tel.OnError(ctx, "reference table sweep failed", err)
				log.Error("reference table sweep failed", "table", t.cfg.Name, "err", err)
			}

		case msg, ok := <-notifications:
			if !ok {
				notifications = nil // subscription closed; the ticker carries on
				continue
			}
			t.handleNotification(ctx, msg)
		}
	}
}

// handleNotification applies a single change immediately. A failure here is
// logged and dropped: the next sweep picks the change up regardless, so this
// path is an optimisation and never the reason the table is correct.
func (t *Table[T]) handleNotification(ctx context.Context, msg *message.Message) {
	log := logger.Get(ctx)
	defer msg.Ack()

	var n struct {
		Urn string
	}
	if err := json.Unmarshal(msg.Payload, &n); err != nil || n.Urn == "" {
		log.Warn("unparseable reference notification, will be picked up by the next sweep",
			"table", t.cfg.Name)
		return
	}
	u, ok := urn.Parse(n.Urn)
	if !ok {
		log.Warn("malformed urn in reference notification", "table", t.cfg.Name, "urn", n.Urn)
		return
	}

	// The single-document endpoint returns the same document shape as the
	// compacted view, so it is parsed the same way.
	body, err := t.bridge.Get(ctx, u)
	if err != nil {
		log.Warn("could not fetch notified reference document, deferring to the next sweep",
			"table", t.cfg.Name, "urn", n.Urn, "err", err)
		return
	}
	r, err := parseRecord[T](body, t.cfg.Key, t.cfg.Decode)
	if err != nil {
		log.Warn("skipping notified reference document",
			"table", t.cfg.Name, "urn", n.Urn, "err", err)
		return
	}
	if t.apply(ctx, r) {
		t.advance(&r.Timestamp)
		logger.Get(ctx).Info("reference table updated from notification",
			"table", t.cfg.Name, "key", r.Key, "keys", t.Len())
		t.notify(ctx, 1)
	}
}

// notify runs the change callback, guarding the consumer's code so a panic in
// it cannot take down the refresher.
func (t *Table[T]) notify(ctx context.Context, applied int) {
	if t.cfg.OnChange == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Get(ctx).Error("reference table OnChange panicked",
				"table", t.cfg.Name, "panic", r)
		}
	}()
	t.cfg.OnChange(ctx, applied)
}

// close releases what the table owns. The qmill subscriber has no Close of its
// own — watermill tears it down when the context passed to Start is cancelled —
// so this exists to give Set a uniform shutdown and a place to hang future
// cleanup.
func (t *Table[T]) close() error { return nil }
