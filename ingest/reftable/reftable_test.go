// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"context"
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

// fakeBridge serves /latest with the same compaction and paging contract as
// raw-data-bridge, over documents shaped exactly as the pipeline stores them.
//
// The shape matters more than the logic. An earlier version of this fake
// emitted an invented envelope, which let two real defects pass unnoticed: the
// payload is base64, and the key is a root-level field rather than part of the
// payload. See e2e_test.go for the check no fake can substitute for.
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

		var since *time.Time
		if s := q.Get("since"); s != "" {
			ts, err := time.Parse(time.RFC3339, s)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			since = &ts
		}
		cursor := q.Get("cursor")
		limit := 1000
		if l := q.Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}

		// newest row per key, with `since` applied before grouping
		latest := map[string]storedRow{}
		for _, d := range f.docs {
			if since != nil && !d.ts.After(*since) {
				continue
			}
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
			next = f.stuckNext
			if len(selected) > limit {
				selected = selected[:limit]
			}
		} else if len(selected) > limit {
			selected = selected[:limit]
			next = selected[len(selected)-1]
		}

		out := make([]json.RawMessage, 0, len(selected))
		var high *time.Time
		for _, k := range selected {
			row := latest[k]
			out = append(out, row.doc)
			if high == nil || row.ts.After(*high) {
				ts := row.ts
				high = &ts
			}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"field": field, "count": len(out),
			"highWater": high, "next": next, "data": out,
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

func TestSweepAppliesOnlyChanges(t *testing.T) {
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
	hw := tbl.HighWater()

	f.add("B", "", payload("second"), now.Add(time.Minute))
	if err := tbl.sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if v, _ := tbl.Get("B"); v.Name != "second" {
		t.Errorf("B = %q, want second", v.Name)
	}
	if v, _ := tbl.Get("A"); v.Name != "Ancient" {
		t.Error("A was lost by an incremental sweep")
	}
	if !tbl.HighWater().After(hw) {
		t.Error("high-water mark did not advance")
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
	if err := tbl.sweep(ctx); err != nil {
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
