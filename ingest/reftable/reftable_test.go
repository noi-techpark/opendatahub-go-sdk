// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
)

// fakeBridge serves the compacted route with the same contract as
// raw-data-bridge, over documents shaped exactly as the pipeline stores them.
//
// The shape matters more than the logic. An earlier version of this fake
// emitted an invented envelope, which let two real defects pass unnoticed: the
// payload is base64, and the key is a root-level field rather than part of the
// payload.
//
// It remains a model of the contract, not evidence about the server. What the
// bridge actually does is pinned on its own side, against a real MongoDB and
// through its real router — infrastructure-v2, raw-data-bridge, the
// *_integration_test.go files. If the two ever disagree, that side is right.
//
// Documents carry the publisher's timestamp, which is what compaction picks the
// newest by. Insertion order is deliberately not modelled: nothing in the client
// depends on it.
type fakeBridge struct {
	mu        sync.Mutex
	docs      []storedRow
	requests  []string
	fail      bool
	stuckNext string
}

type storedRow struct {
	key string
	ts  time.Time
	doc json.RawMessage
}

func (f *fakeBridge) add(key, op, payload string, ts time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, storedRow{key, ts, storedDoc(key, op, "test.v1", payload, ts)})
}

func (f *fakeBridge) addRaw(key string, ts time.Time, doc json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, storedRow{key, ts, doc})
}

// remove models an operator deleting a record outright, rather than publishing a
// tombstone for it.
func (f *fakeBridge) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.docs[:0]
	for _, d := range f.docs {
		if d.key != key {
			kept = append(kept, d)
		}
	}
	f.docs = kept
}

// encodeToken keeps the fake's cursor opaque, as the real one is: a fake whose
// cursor is a bare key lets a client get away with parsing something it must not.
func encodeToken(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeToken(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (f *fakeBridge) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.requests = append(f.requests, r.URL.String())
		if f.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Only the real route shape is served. A fake that answers whatever it
		// is asked is how a client ends up calling a URL that does not exist in
		// production while its tests stay green.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[2] != "compacted" || parts[3] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		field := parts[3]

		q := r.URL.Query()

		limit := 1000
		if l := q.Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}

		cursor := ""
		if raw := q.Get("cursor"); raw != "" {
			key, ok := decodeToken(raw)
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			cursor = key
		}

		// newest row per key, by the publisher's timestamp
		latest := map[string]storedRow{}
		for _, d := range f.docs {
			if prev, ok := latest[d.key]; !ok || !d.ts.Before(prev.ts) {
				latest[d.key] = d
			}
		}

		keys := make([]string, 0, len(latest))
		for k := range latest {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		selected := make([]string, 0, len(keys))
		for _, k := range keys {
			if cursor != "" && k <= cursor {
				continue
			}
			selected = append(selected, k)
			if len(selected) > limit {
				break
			}
		}

		next := ""
		if f.stuckNext != "" {
			next = encodeToken(f.stuckNext)
			if len(selected) > limit {
				selected = selected[:limit]
			}
		} else if len(selected) > limit {
			selected = selected[:limit]
			next = encodeToken(selected[len(selected)-1])
		}

		out := make([]json.RawMessage, 0, len(selected))
		for _, k := range selected {
			out = append(out, latest[k].doc)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"field": field, "count": len(out), "next": next, "data": out,
		})
	}
}

func newTable(t *testing.T, f *fakeBridge, cfg Config) (*Table[enrichment], *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	bridge := rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: srv.URL})
	if cfg.DB == "" {
		cfg.DB, cfg.Collection, cfg.Key = "enrichment", "parking", "key"
	}
	if cfg.Decode == nil {
		// storedDoc base64-encodes its payload, so these tests exercise the
		// legacy encoding rather than the DecodeText default.
		cfg.Decode = DecodeBase64
	}
	return New[enrichment](bridge, cfg), srv
}

func payload(name string) string { return fmt.Sprintf(`{"name":%q}`, name) }

func TestBootstrapCompactsAndIsComplete(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	// A written four years ago and never touched; B superseded twice.
	f.add("A", "", payload("Ancient"), now.AddDate(-4, 0, 0))
	f.add("B", "", payload("old"), now.AddDate(-4, 0, 0))
	f.add("B", "", payload("newer"), now.Add(-48*time.Hour))
	f.add("B", "", payload("newest"), now.Add(-time.Hour))
	f.add("C", "", payload("Recent"), now.Add(-30*time.Minute))

	tbl, srv := newTable(t, f, Config{Schema: "test.v1"})
	defer srv.Close()

	if err := tbl.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if tbl.Len() != 3 {
		t.Fatalf("Len = %d, want 3", tbl.Len())
	}
	if v, _ := tbl.Get("A"); v.Name != "Ancient" {
		t.Errorf("A = %q, want Ancient — a key written long ago must survive bootstrap", v.Name)
	}
	if v, _ := tbl.Get("B"); v.Name != "newest" {
		t.Errorf("B = %q, want newest", v.Name)
	}
	select {
	case <-tbl.Ready():
	default:
		t.Error("Ready not closed after a successful bootstrap")
	}
}

func TestBootstrapPagesThroughEveryKey(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	for _, k := range []string{"A", "B", "C", "D", "E"} {
		f.add(k, "", payload("name-"+k), now.Add(-time.Hour))
	}

	tbl, srv := newTable(t, f, Config{PageLimit: 2})
	defer srv.Close()

	if err := tbl.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if tbl.Len() != 5 {
		t.Fatalf("Len = %d, want 5 — a paged bootstrap must still be complete", tbl.Len())
	}
	for _, k := range []string{"A", "B", "C", "D", "E"} {
		if v, ok := tbl.Get(k); !ok || v.Name != "name-"+k {
			t.Errorf("key %s missing or wrong: %+v", k, v)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(strings.Join(f.requests, " "), "cursor=") {
		t.Error("no request carried a cursor, so paging was not exercised")
	}
}

// A key field that does not exist yields an unchanging cursor. The walk must
// fail loudly instead of looping.
func TestBootstrapRejectsNonAdvancingCursor(t *testing.T) {
	f := &fakeBridge{stuckNext: "STUCK"}
	f.add("A", "", payload("a"), time.Now().UTC())

	tbl, srv := newTable(t, f, Config{PageLimit: 1})
	defer srv.Close()

	err := tbl.bootstrap(context.Background())
	if err == nil {
		t.Fatal("bootstrap succeeded despite a cursor that never advances")
	}
	if !strings.Contains(err.Error(), "cursor did not advance") {
		t.Errorf("error = %v, want it to name the stuck cursor", err)
	}
}

func TestReconcileAppliesOnlyChanges(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	f.add("A", "", payload("Ancient"), now.AddDate(-4, 0, 0))
	f.add("B", "", payload("first"), now.Add(-time.Hour))

	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()
	ctx := context.Background()

	if err := tbl.bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	f.add("B", "", payload("second"), now.Add(time.Minute))
	changed, err := tbl.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1: only B was edited", changed)
	}

	if v, _ := tbl.Get("B"); v.Name != "second" {
		t.Errorf("B = %q, want second", v.Name)
	}
	if v, _ := tbl.Get("A"); v.Name != "Ancient" {
		t.Error("A was lost by an incremental sweep")
	}
	// A reconcile that finds nothing new must report nothing changed, or every
	// consumer rebuilds its world every few minutes for no reason.
	if changed, err := tbl.reconcile(ctx); err != nil || changed != 0 {
		t.Errorf("second reconcile: changed = %d, err = %v; want 0, nil", changed, err)
	}
}

func TestDeleteTombstoneRemovesKey(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	f.add("A", "", payload("present"), now.Add(-time.Hour))

	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()
	ctx := context.Background()
	if err := tbl.bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if tbl.Len() != 1 {
		t.Fatalf("Len = %d, want 1", tbl.Len())
	}

	f.add("A", OpDelete, "", now)
	if _, err := tbl.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := tbl.Get("A"); ok {
		t.Error("key still present after a delete tombstone")
	}
}

func TestOutOfOrderRecordIsIgnored(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()
	ctx := context.Background()

	newer := record[enrichment]{Key: "A", Timestamp: now, Data: enrichment{Name: "newer"}}
	older := record[enrichment]{Key: "A", Timestamp: now.Add(-time.Hour), Data: enrichment{Name: "older"}}

	if !tbl.apply(ctx, newer) {
		t.Fatal("newer record was not applied")
	}
	if tbl.apply(ctx, older) {
		t.Error("older record was applied over a newer one")
	}
	if v, _ := tbl.Get("A"); v.Name != "newer" {
		t.Errorf("A = %q, want newer", v.Name)
	}
}

func TestRefusesForeignSchemaAndUnknownOp(t *testing.T) {
	ctx := context.Background()
	f := &fakeBridge{}
	tbl, srv := newTable(t, f, Config{Schema: "test.v1"})
	defer srv.Close()

	foreign := record[enrichment]{Key: "A", Schema: "other.v9", Data: enrichment{Name: "x"}}
	if tbl.apply(ctx, foreign) {
		t.Error("applied a record with a foreign schema")
	}
	unknownOp := record[enrichment]{Key: "B", Schema: "test.v1", Op: "frobnicate"}
	if tbl.apply(ctx, unknownOp) {
		t.Error("applied a record with an unknown op")
	}
	if tbl.Len() != 0 {
		t.Errorf("Len = %d, want 0", tbl.Len())
	}
}

// One malformed document must not deny the consumer every other key.
func TestMalformedDocumentIsSkippedNotFatal(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	f.add("A", "", payload("good"), now.Add(-time.Hour))
	f.addRaw("B", now, json.RawMessage(`{"meta":{"key":"B"},"rawdata":"!!!not base64!!!"}`))

	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()

	if err := tbl.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap failed because of one bad document: %v", err)
	}
	if _, ok := tbl.Get("A"); !ok {
		t.Error("a good key was lost because another document was malformed")
	}
	if _, ok := tbl.Get("B"); ok {
		t.Error("the malformed document was applied")
	}
}

func TestBootstrapFailurePropagates(t *testing.T) {
	f := &fakeBridge{fail: true}
	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()

	if err := tbl.bootstrap(context.Background()); err == nil {
		t.Fatal("bootstrap succeeded against a failing bridge")
	}
	select {
	case <-tbl.Ready():
		t.Error("Ready closed despite a failed bootstrap")
	default:
	}
}

func TestSetStartFailsIfAnyTableFails(t *testing.T) {
	good := &fakeBridge{}
	good.add("A", "", payload("ok"), time.Now().UTC())
	bad := &fakeBridge{fail: true}

	t1, s1 := newTable(t, good, Config{Name: "good"})
	defer s1.Close()
	t2, s2 := newTable(t, bad, Config{Name: "bad"})
	defer s2.Close()

	if err := NewSet(t1, t2).Start(context.Background()); err == nil {
		t.Fatal("Set.Start succeeded although one table failed to bootstrap")
	}
}

// A table that declares an expected schema refuses anything else, including a
// record that carries no version at all. The rules were written against one
// shape, and an unstamped record is not evidence of anything.
func TestSchemaMismatchAndUnstampedRecordsAreRefused(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	f.add("good", "", payload("kept"), now)
	f.addRaw("unstamped", now, storedDoc("unstamped", "", "", payload("no version"), now))
	f.addRaw("wrong", now, storedDoc("wrong", "", "other.v9", payload("wrong version"), now))

	tbl, srv := newTable(t, f, Config{Schema: "test.v1"})
	defer srv.Close()

	if err := tbl.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, ok := tbl.Get("good"); !ok {
		t.Error("a correctly stamped record was refused")
	}
	if _, ok := tbl.Get("unstamped"); ok {
		t.Error("a record with no schema version was accepted")
	}
	if _, ok := tbl.Get("wrong"); ok {
		t.Error("a record with the wrong schema version was accepted")
	}
}

// A reconcile re-reads everything, so a document's timestamp cannot hide it.
//
// A collector may publish at any moment carrying any timestamp: raw-writer-2
// takes it from the request path, so a record written today may be dated last
// year. Any incremental catch-up keyed on that timestamp steps straight over it
// and never comes back. Re-reading the collection has no such failure mode, and
// this test is the reason there is no incremental path to maintain.
func TestReconcileSeesADocumentDatedInThePast(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	f.add("A", "", payload("current"), now)

	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()
	ctx := context.Background()

	if err := tbl.bootstrap(ctx); err != nil {
		t.Fatal(err)
	}

	// Written now, dated a year ago — below any event-time mark the bootstrap
	// could have recorded.
	f.add("B", "", payload("backfilled"), now.AddDate(-1, 0, 0))

	if _, err := tbl.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if v, ok := tbl.Get("B"); !ok || v.Name != "backfilled" {
		t.Error("a document written after the mark but dated before it never reached the table; " +
			"the sweep is resuming on the publisher's clock")
	}
}

// A key that disappears from the collection has to disappear from the table.
//
// Merging each read into the live map instead of replacing it left such a key
// behind for the life of the process: nothing ever removed it, because nothing
// ever came back to say it was gone. Only an explicit tombstone could, and an
// operator who deletes a record outright never publishes one.
func TestReconcileDropsKeysThatLeftTheCollection(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	f.add("A", "", payload("kept"), now)
	f.add("B", "", payload("doomed"), now)

	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()
	ctx := context.Background()
	if err := tbl.bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if tbl.Len() != 2 {
		t.Fatalf("Len = %d, want 2", tbl.Len())
	}

	f.remove("B")
	changed, err := tbl.reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tbl.Get("B"); ok {
		t.Error("a key that is no longer in the collection is still in the table")
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1: the removal is a change", changed)
	}
	if _, ok := tbl.Get("A"); !ok {
		t.Error("the surviving key was dropped too")
	}
}

// A reconcile that fails part way must leave the previous table serving.
//
// It builds the replacement beside the live one and swaps at the end, so there
// is no window in which a reader sees a half-built map — and a failed read
// changes nothing at all.
func TestFailedReconcileLeavesTheTableIntact(t *testing.T) {
	now := time.Now().UTC()
	f := &fakeBridge{}
	f.add("A", "", payload("one"), now.Add(-time.Hour))

	tbl, srv := newTable(t, f, Config{})
	defer srv.Close()
	ctx := context.Background()
	if err := tbl.bootstrap(ctx); err != nil {
		t.Fatal(err)
	}

	f.add("B", "", payload("two"), now)
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()

	if _, err := tbl.reconcile(ctx); err == nil {
		t.Fatal("a failing reconcile reported success")
	}
	if v, ok := tbl.Get("A"); !ok || v.Name != "one" {
		t.Error("a failed reconcile emptied or corrupted the table")
	}

	// And once the bridge recovers, nothing has been lost.
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	if _, err := tbl.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := tbl.Get("B"); !ok {
		t.Error("the change missed by the failed reconcile was never picked up")
	}
}

// A reconcile that keeps failing has to become visible. It used to leave nothing
// behind but a repeating error line: the table went on serving what it last had,
// no probe could tell, and nothing said how long it had been wrong.
func TestSustainedReconcileFailureIsReportedAsStale(t *testing.T) {
	f := &fakeBridge{}
	f.add("A", "", payload("one"), time.Now().UTC())

	tbl, srv := newTable(t, f, Config{Sweep: time.Hour, StaleAfter: time.Millisecond})
	defer srv.Close()
	ctx := context.Background()
	set := NewSet(tbl)

	if err := tbl.bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Health(); err != nil {
		t.Fatalf("a freshly bootstrapped table is unhealthy: %v", err)
	}
	if tbl.LastReconcile().IsZero() {
		t.Error("bootstrap recorded no reconcile time")
	}

	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	time.Sleep(2 * time.Millisecond) // past the budget
	tbl.refresh(ctx)

	err := tbl.Health()
	if err == nil {
		t.Fatal("a table that has stopped reconciling reports itself healthy")
	}
	// The message has to carry how long, or an operator sees the same line
	// whether the table is a minute behind or a week.
	if !strings.Contains(err.Error(), "no successful reconcile for") {
		t.Errorf("health error %q does not say how long the table has been behind", err)
	}
	if serr := set.Healthy(); serr == nil || !strings.Contains(serr.Error(), tbl.Name()) {
		t.Errorf("Set.Healthy = %v, want it to name the stale table", serr)
	}
	// It still serves: being behind degrades the output, it does not empty it.
	if _, ok := tbl.Get("A"); !ok {
		t.Error("a stale table stopped answering lookups")
	}

	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	tbl.refresh(ctx)
	if err := tbl.Health(); err != nil {
		t.Errorf("the table recovered but still reports %v", err)
	}
	if err := set.Healthy(); err != nil {
		t.Errorf("Set.Healthy = %v after recovery", err)
	}
}

// A bridge that accepts the connection and never answers must not wedge the
// refresher. The SDK's HTTP client sets no timeout, so without a deadline of its
// own the loop would block on the socket — and since the loop is what reports
// failures, the table would go quiet rather than go loud.
func TestAHangingBridgeCannotWedgeTheRefresher(t *testing.T) {
	// A handler that answers far later than the reconcile is allowed to wait. It
	// sleeps rather than blocking on a channel, because Close waits for
	// outstanding handlers and a handler that never returns wedges the test
	// itself rather than the code under test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	tbl := New[enrichment](
		rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: srv.URL}),
		Config{Name: "hanging", DB: "enrichment", Collection: "parking", Key: "key",
			Sweep: time.Hour, ReconcileTimeout: 200 * time.Millisecond,
			StaleAfter: time.Millisecond})

	done := make(chan struct{})
	go func() {
		defer close(done)
		tbl.refresh(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not return: a hung bridge blocks the refresh loop indefinitely")
	}
	if err := tbl.Health(); err == nil {
		t.Error("a table that never reconciled reports itself healthy")
	}
}

// The defaults have to be usable without thinking about them.
func TestStalenessDefaultsAreDerivedFromSweep(t *testing.T) {
	tbl := New[enrichment](nil, Config{DB: "d", Collection: "c", Key: "k"})
	if tbl.cfg.StaleAfter != DefaultStaleFactor*DefaultSweep {
		t.Errorf("StaleAfter = %s, want %s", tbl.cfg.StaleAfter, DefaultStaleFactor*DefaultSweep)
	}
	if tbl.cfg.ReconcileTimeout != DefaultSweep {
		t.Errorf("ReconcileTimeout = %s, want %s", tbl.cfg.ReconcileTimeout, DefaultSweep)
	}
	// A short sweep must not start cutting reconciles off at the knees.
	short := New[enrichment](nil, Config{DB: "d", Collection: "c", Key: "k", Sweep: time.Second})
	if short.cfg.ReconcileTimeout != MinReconcileTimeout {
		t.Errorf("ReconcileTimeout = %s with a 1s sweep, want the %s floor",
			short.cfg.ReconcileTimeout, MinReconcileTimeout)
	}
}
