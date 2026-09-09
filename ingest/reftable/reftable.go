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
//   - a reconcile reads the compacted view of a collection (the newest document
//     per key), builds an in-memory map and swaps it in;
//   - that same reconcile runs again on a timer, which is what keeps the table
//     correct;
//   - an optional queue subscription applies single changes as they happen,
//     which only affects latency.
//
// A table that stops reconciling keeps serving whatever it last had, so
// staleness is state rather than a log line: see Table.Health and Set.Healthy,
// which is what a liveness or readiness probe should read.
//
// There is no incremental catch-up, on purpose. A reference table is operator
// configuration — tens to hundreds of rows, one page of one HTTP response — so
// re-reading all of it is cheap, and "we read everything again" is a correctness
// argument that fits in a sentence and cannot be got subtly wrong. Anything
// clever here would be defending a cost nobody is paying.
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
	"reflect"
	"sync"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/urn"
	"github.com/noi-techpark/opendatahub-go-sdk/qmill"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
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

	// Sweep is how often the table re-reads the collection. This is the
	// mechanism that guarantees correctness; the queue only reduces latency.
	// Defaults to DefaultSweep.
	Sweep time.Duration

	// ReconcileTimeout bounds one reconcile. Defaults to Sweep, with a floor of
	// MinReconcileTimeout.
	//
	// It exists because the SDK's HTTP client sets no timeout of its own: a
	// bridge that accepts the connection and then never answers would block the
	// refresher for as long as the socket stayed open, and since the loop is
	// what logs failures, nothing would say so. A table stuck that way is worse
	// than one that fails — it is one that has stopped even trying, silently.
	ReconcileTimeout time.Duration

	// StaleAfter is how long the table may go without a successful reconcile
	// before Health reports it unhealthy. Defaults to DefaultStaleFactor × Sweep,
	// which absorbs a couple of transient failures and no more.
	//
	// Crossing it does not stop anything. A reference table that is behind
	// degrades the output; a transformer that exits stops it altogether, and
	// that trade is the consumer's to make — wire Set.Healthy into a probe.
	StaleAfter time.Duration

	// PageLimit caps documents per request. Defaults to DefaultPageLimit, which
	// is more rows than a reference table is expected to have.
	PageLimit int

	// MQ_* enable live change notifications. Leave MQ_URI empty to run
	// reconcile-only, which is a supported configuration rather than a degraded
	// one — the queue never makes the table correct, only current sooner.
	//
	// MQ_KEY is the routing key the router publishes this collection under, and
	// it is "<db>.<collection>": the router takes the URN's namespace without the
	// document id and joins it with '.'. MQ_EXCHANGE is the router's output
	// exchange, `routed`. Getting the key wrong binds a queue that never
	// receives anything, and nothing says so — the table simply falls back to
	// reconciling, which is the whole reason that fallback exists.
	MQ_URI      string
	MQ_CLIENT   string
	MQ_EXCHANGE string
	MQ_QUEUE    string
	MQ_KEY      string
}

const (
	DefaultSweep     = 5 * time.Minute
	DefaultPageLimit = 1000

	// MinReconcileTimeout floors ReconcileTimeout, so a deliberately short Sweep
	// does not start cutting off reconciles that were about to succeed.
	MinReconcileTimeout = 30 * time.Second

	// DefaultStaleFactor sets StaleAfter to this many sweep intervals.
	DefaultStaleFactor = 3
	// maxWalkPages bounds a walk so a mis-specified key that never advances the
	// cursor fails loudly instead of looping forever.
	maxWalkPages = 10000
)

// Table is a keyed, in-memory view of a reference collection. Reads are safe
// from the message handler while refreshes run.
type Table[T any] struct {
	cfg    Config
	bridge *rdb.RDBridge

	mu     sync.RWMutex
	data   map[string]T
	stamps map[string]time.Time // per key: event time of the document applied
	ready  bool

	// Staleness is state, not a log line. A reconcile that fails forever used to
	// leave nothing behind but a repeating error with no indication of how long
	// the table had been wrong, and nothing a probe could read.
	lastReconcile time.Time
	lastErr       error
	stale         bool

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
	if cfg.ReconcileTimeout <= 0 {
		cfg.ReconcileTimeout = max(cfg.Sweep, MinReconcileTimeout)
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = DefaultStaleFactor * cfg.Sweep
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

// Ready is closed once the first bootstrap has succeeded.
func (t *Table[T]) Ready() <-chan struct{} { return t.readyCh }

// LastReconcile is when the table last read the collection successfully.
func (t *Table[T]) LastReconcile() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastReconcile
}

// Health reports nil while the table is current, and otherwise says how long it
// has been out of date and why.
//
// It is the signal a liveness or readiness probe should read: the table keeps
// serving what it last had, so nothing else reveals that it stopped being
// corrected.
func (t *Table[T]) Health() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.ready {
		return fmt.Errorf("%s: not bootstrapped", t.cfg.Name)
	}
	// Compared unrounded, reported rounded: rounding first made every budget
	// shorter than a second unreachable.
	age := time.Since(t.lastReconcile)
	if age <= t.cfg.StaleAfter {
		return nil
	}
	shown := t.staleFor()
	if t.lastErr != nil {
		return fmt.Errorf("%s: no successful reconcile for %s: %w", t.cfg.Name, shown, t.lastErr)
	}
	return fmt.Errorf("%s: no successful reconcile for %s", t.cfg.Name, shown)
}

// staleFor renders how long the table has been behind. Callers hold the lock.
//
// A table that has never reconciled has a zero timestamp, and reporting the age
// of that verbatim gives 2562047h — a number that says nothing except that
// somebody printed a zero time.
func (t *Table[T]) staleFor() string {
	if t.lastReconcile.IsZero() {
		return "ever"
	}
	return time.Since(t.lastReconcile).Round(time.Second).String()
}

// staleTables counts the tables currently past their staleness budget. It is
// moved on transition rather than set on every tick, so the value is the number
// of tables in trouble rather than a count of failures.
//
// Built lazily: telemetry is configured in main, after package initialisation.
var (
	staleTablesOnce sync.Once
	staleTables     otelmetric.Int64UpDownCounter
)

func staleTablesCounter() otelmetric.Int64UpDownCounter {
	staleTablesOnce.Do(func() {
		c, err := tel.MeterInt64UpDownCounter(tel.Metric{
			Name:        "reftable_stale",
			Unit:        "{count}",
			Description: "Reference tables with no successful reconcile inside their staleness budget.",
		})
		if err == nil {
			staleTables = c
		}
	})
	return staleTables
}

// recordSuccess marks a completed reconcile and clears any staleness.
func (t *Table[T]) recordSuccess(ctx context.Context) {
	t.mu.Lock()
	wasStale := t.stale
	t.lastReconcile = time.Now()
	t.lastErr = nil
	t.stale = false
	t.mu.Unlock()

	if wasStale {
		if c := staleTablesCounter(); c != nil {
			c.Add(ctx, -1, otelmetric.WithAttributes(attribute.String("table", t.cfg.Name)))
		}
		logger.Get(ctx).Info("reference table is current again", "table", t.cfg.Name)
	}
}

// recordFailure remembers why the table is behind and how far, and reports the
// moment it crosses its budget.
func (t *Table[T]) recordFailure(ctx context.Context, err error) {
	t.mu.Lock()
	t.lastErr = err
	age := time.Since(t.lastReconcile)
	crossed := !t.stale && age > t.cfg.StaleAfter
	if crossed {
		t.stale = true
	}
	stale := t.stale
	shown := t.staleFor()
	t.mu.Unlock()

	log := logger.Get(ctx)
	tel.OnError(ctx, "reference table reconcile failed", err)
	if crossed {
		if c := staleTablesCounter(); c != nil {
			c.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("table", t.cfg.Name)))
		}
	}
	// Below the budget this is noise a retry will clear; above it, the table is
	// serving data known to be out of date and the duration is the point.
	if stale {
		log.Error("reference table is stale",
			"table", t.cfg.Name, "stale_for", shown,
			"budget", t.cfg.StaleAfter.String(), "err", err)
		return
	}
	log.Warn("reference table reconcile failed, will retry",
		"table", t.cfg.Name, "stale_for", shown, "err", err)
}

// refresh runs one periodic reconcile and records what happened.
func (t *Table[T]) refresh(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, t.cfg.ReconcileTimeout)
	defer cancel()

	changed, err := t.reconcile(runCtx)
	if err != nil {
		t.recordFailure(ctx, err)
		return
	}
	t.recordSuccess(ctx)
	if changed > 0 {
		logger.Get(ctx).Info("reference table updated",
			"table", t.cfg.Name, "changed", changed, "keys", t.Len())
		t.notify(ctx, changed)
	}
}

// reconcile reads the whole compacted view and replaces the table with it.
//
// This is both the initial load and the periodic correction, because they are
// the same operation. The walk is the entire current state of the collection, so
// re-reading it cannot miss a change: not one whose notification was dropped,
// not one written with an old timestamp, not one written while a previous
// reconcile was running. There is no incremental path to get subtly wrong and
// nothing to resume from.
//
// The new table is built beside the live one and swapped, rather than merged
// into it. That way a reader never sees a half-built map, and a key that has
// gone from the collection goes from here too — merging in place leaves such a
// key behind for the life of the process.
//
// It returns how many keys actually differ, so a consumer's OnChange fires for
// real edits rather than for every re-read.
func (t *Table[T]) reconcile(ctx context.Context) (int, error) {
	log := logger.Get(ctx)
	next := map[string]T{}
	stamps := map[string]time.Time{}

	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= maxWalkPages {
			return 0, fmt.Errorf("exceeded %d pages", maxWalkPages)
		}
		page, err := t.bridge.GetCompacted(ctx, rdb.CompactedQuery{
			DB:         t.cfg.DB,
			Collection: t.cfg.Collection,
			Field:      t.cfg.Key,
			Cursor:     cursor,
			Limit:      t.cfg.PageLimit,
		})
		if err != nil {
			return 0, err
		}

		for _, item := range page.Items {
			r, err := parseRecord[T](item, t.cfg.Key, t.cfg.Decode)
			if err != nil {
				// One malformed record must not deny the consumer every other key.
				log.Warn("skipping reference document", "table", t.cfg.Name, "err", err)
				continue
			}
			if !t.accepts(ctx, r) {
				continue
			}
			// A tombstone is simply absent from the rebuilt table. Compaction
			// serves it as the key's newest document, which is exactly the
			// signal that the key is gone.
			if r.Op == OpDelete {
				continue
			}
			next[r.Key] = r.Data
			stamps[r.Key] = r.Timestamp
		}

		if page.Next == "" {
			break
		}
		if page.Next == cursor {
			return 0, fmt.Errorf("cursor did not advance past %q, check that meta.%s is a real field on the stored documents",
				cursor, t.cfg.Key)
		}
		cursor = page.Next
	}

	t.mu.Lock()
	changed := countChanges(t.data, next)
	// A notification applied between the last page and this swap is discarded
	// with the old map. It was at most Sweep seconds of latency, and the next
	// reconcile picks the change up regardless — the same guarantee that lets
	// the queue path fail silently.
	t.data, t.stamps = next, stamps
	t.mu.Unlock()
	return changed, nil
}

// countChanges reports how many keys differ between two versions of the table.
func countChanges[T any](old, next map[string]T) int {
	n := 0
	for k, v := range next {
		prev, ok := old[k]
		if !ok || !reflect.DeepEqual(prev, v) {
			n++
		}
	}
	for k := range old {
		if _, ok := next[k]; !ok {
			n++
		}
	}
	return n
}

// bootstrap loads the table for the first time.
func (t *Table[T]) bootstrap(ctx context.Context) error {
	// Bounded like any other reconcile: a bootstrap that hangs holds up start-up
	// with no more explanation than one that fails.
	runCtx, cancel := context.WithTimeout(ctx, t.cfg.ReconcileTimeout)
	defer cancel()

	if _, err := t.reconcile(runCtx); err != nil {
		// A missing collection is a configuration error, not a cold start.
		// Bootstrap is fail-closed precisely so this stops the process instead
		// of producing an empty table that overwrites live data.
		if errors.Is(err, rdb.ErrCollectionNotFound) {
			return fmt.Errorf("bootstrap %s: %w; check DB=%q Collection=%q against what the publisher writes",
				t.cfg.Name, err, t.cfg.DB, t.cfg.Collection)
		}
		return fmt.Errorf("bootstrap %s: %w", t.cfg.Name, err)
	}

	t.mu.Lock()
	t.ready = true
	t.mu.Unlock()
	t.recordSuccess(ctx)
	t.readyOnce.Do(func() { close(t.readyCh) })

	logger.Get(ctx).Info("reference table bootstrapped",
		"table", t.cfg.Name, "keys", t.Len())
	return nil
}

// accepts reports whether a record may be applied, warning about the ones that
// may not. Shared by the reconcile and the queue path so a record cannot be
// refused by one and accepted by the other.
func (t *Table[T]) accepts(ctx context.Context, r record[T]) bool {
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
	switch r.Op {
	case "", OpUpsert, OpDelete:
		return true
	default:
		log.Warn("reference document with unknown op, skipped",
			"table", t.cfg.Name, "key", r.Key, "op", r.Op)
		return false
	}
}

// apply installs one record into the live table. Only the queue path uses it;
// a reconcile builds a whole new table instead.
func (t *Table[T]) apply(ctx context.Context, r record[T]) bool {
	log := logger.Get(ctx)
	if !t.accepts(ctx, r) {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Event time decides which revision of a key wins. That is the one job it is
	// right for: a catch-up returns the newest document per key *within* its
	// window, so a backfill can come back as the newest thing written since the
	// mark while carrying an older timestamp than the revision already held.
	// Applying it would be a silent regression of that key.
	if prev, ok := t.stamps[r.Key]; ok && r.Timestamp.Before(prev) {
		log.Debug("ignoring older reference document",
			"table", t.cfg.Name, "key", r.Key, "got", r.Timestamp, "have", prev)
		return false
	}

	if r.Op == OpDelete {
		delete(t.data, r.Key)
	} else {
		t.data[r.Key] = r.Data
	}
	t.stamps[r.Key] = r.Timestamp
	return true
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

// run consumes notifications and reconciles on a ticker until ctx is cancelled.
func (t *Table[T]) run(ctx context.Context) {
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
			t.refresh(ctx)

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
