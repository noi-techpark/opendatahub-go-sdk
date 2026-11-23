// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
)

const testProvider = "famas/parking-discovery"

type captured struct {
	path   string
	key    string
	schema string
	ctype  string
	body   []byte
}

func (c captured) record(t *testing.T) Record {
	t.Helper()
	var r Record
	if err := json.Unmarshal(c.body, &r); err != nil {
		t.Fatalf("body is not a Record: %v: %s", err, c.body)
	}
	return r
}

// fakeWriter stands in for raw-writer-2: it records what it is sent and fails
// the next `failures` requests.
type fakeWriter struct {
	mu       sync.Mutex
	got      []captured
	failures int
}

func (f *fakeWriter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.got = append(f.got, captured{
		path:   r.URL.Path,
		key:    r.Header.Get("X-OpenDataHub-key"),
		schema: r.Header.Get("X-OpenDataHub-schema"),
		ctype:  r.Header.Get("Content-Type"),
		body:   body,
	})
}

func (f *fakeWriter) writes() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.got...)
}

func (f *fakeWriter) keys() []string {
	var out []string
	for _, c := range f.writes() {
		out = append(out, c.key)
	}
	return out
}

// fakeBridge serves the compacted route of raw-data-bridge for one collection,
// with documents shaped as raw-writer-2 stores them: the key and schema under
// meta, the JSON body as a string in rawdata.
type fakeBridge struct {
	db, collection string
	exists         bool
	docs           map[string]Record
	other          []json.RawMessage
}

func (f *fakeBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := fmt.Sprintf("/%s/%s/compacted/%s", f.db, f.collection, KeyField)
	if r.URL.Path != want || !f.exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	data := append([]json.RawMessage(nil), f.other...)
	for key, rec := range f.docs {
		body, _ := json.Marshal(rec)
		doc, _ := json.Marshal(map[string]any{
			"meta":      map[string]string{"key": key, "schema": Schema},
			"timestamp": rec.Changed.Format(time.RFC3339Nano),
			"rawdata":   string(body),
		})
		data = append(data, doc)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"field": KeyField, "count": len(data), "next": "", "data": data,
	})
}

type harness struct {
	t      *testing.T
	writer *fakeWriter
	bridge *fakeBridge
	clock  *time.Time
	r      *Reporter
}

func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, writer: &fakeWriter{}}
	h.bridge = &fakeBridge{db: "famas", collection: "parking-discovery", exists: true, docs: map[string]Record{}}

	wsrv := httptest.NewServer(h.writer)
	bsrv := httptest.NewServer(h.bridge)
	t.Cleanup(wsrv.Close)
	t.Cleanup(bsrv.Close)

	cfg := Config{Provider: testProvider, RawWriterURL: wsrv.URL}
	if mod != nil {
		mod(&cfg)
	}
	h.r = New(rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: bsrv.URL}), cfg)
	start := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	h.clock = &start
	h.r.now = func() time.Time { return *h.clock }
	return h
}

func (h *harness) advance(d time.Duration) { *h.clock = h.clock.Add(d) }

func (h *harness) start() {
	h.t.Helper()
	if err := h.r.Start(context.Background()); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
}

func (h *harness) flush() {
	h.t.Helper()
	if err := h.r.Flush(context.Background()); err != nil {
		h.t.Fatalf("Flush: %v", err)
	}
}

func f64(v float64) *float64 { return &v }

func published(name string) Entry {
	return Entry{Status: StatusPublished, Reason: "no-record", Name: name, Lat: f64(46.5), Lon: f64(11.3),
		Data: map[string]any{"capacity": 120, "names": []string{"a", "b"}}}
}

func TestFirstObservePublishesOnceAndCallsOnNew(t *testing.T) {
	var newKeys []string
	h := newHarness(t, func(c *Config) {
		c.OnNew = func(_ context.Context, key string, r Record) {
			newKeys = append(newKeys, key+":"+string(r.Status))
		}
	})
	h.start()

	h.r.Observe("P1", published("Laurin"))
	h.r.Observe("P1", published("Laurin"))
	h.flush()

	got := h.writer.writes()
	if len(got) != 1 {
		t.Fatalf("writes = %d, want 1", len(got))
	}
	w := got[0]
	if want := "/famas/parking-discovery/2026-05-01T10:00:00Z"; w.path != want {
		t.Errorf("path = %q, want %q", w.path, want)
	}
	if w.key != "P1" || w.schema != Schema {
		t.Errorf("headers key=%q schema=%q", w.key, w.schema)
	}
	if !strings.HasPrefix(w.ctype, "application/json") {
		t.Errorf("content type = %q", w.ctype)
	}
	rec := w.record(t)
	if rec.Status != StatusPublished || rec.Reason != "no-record" || rec.Name != "Laurin" {
		t.Errorf("record = %+v", rec)
	}
	if !rec.FirstSeen.Equal(*h.clock) || !rec.Changed.Equal(*h.clock) {
		t.Errorf("FirstSeen/Changed = %v/%v, want %v", rec.FirstSeen, rec.Changed, *h.clock)
	}
	if rec.Lat == nil || *rec.Lat != 46.5 || rec.Data["capacity"] != float64(120) {
		t.Errorf("lat/data lost: %+v", rec)
	}
	if !reflect.DeepEqual(newKeys, []string{"P1:published"}) {
		t.Errorf("OnNew calls = %v, want exactly one", newKeys)
	}
}

func TestUnchangedReobserveWritesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.r.Observe("P1", published("Laurin"))
	h.flush()

	h.advance(time.Hour)
	h.r.Observe("P1", published("Laurin"))
	h.flush()

	if n := len(h.writer.writes()); n != 1 {
		t.Errorf("writes = %d, want 1", n)
	}
}

func TestChangedWritesAndKeepsFirstSeen(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	first := *h.clock
	h.r.Observe("P1", Entry{Status: StatusWithheld, Reason: "no-record", Name: "Laurin"})
	h.flush()

	h.advance(time.Hour)
	h.r.Observe("P1", Entry{Status: StatusPublished, Name: "Laurin"})
	h.flush()

	got := h.writer.writes()
	if len(got) != 2 {
		t.Fatalf("writes = %d, want 2", len(got))
	}
	rec := got[1].record(t)
	if rec.Status != StatusPublished || rec.Reason != "" {
		t.Errorf("record = %+v", rec)
	}
	if !rec.FirstSeen.Equal(first) {
		t.Errorf("FirstSeen = %v, want %v", rec.FirstSeen, first)
	}
	if !rec.Changed.Equal(*h.clock) {
		t.Errorf("Changed = %v, want %v", rec.Changed, *h.clock)
	}
}

func TestWriteTimestampsOfAKeyStrictlyIncrease(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.r.Observe("P1", published("a"))
	h.flush()
	h.r.Observe("P1", published("b"))
	h.flush()

	got := h.writer.writes()
	if len(got) != 2 || got[0].path == got[1].path {
		t.Errorf("two writes of a key in one second share a path: %v", got)
	}
}

func TestFailedWriteIsRetriedAndOthersContinue(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.r.Observe("A", published("a"))
	h.r.Observe("B", published("b"))
	h.writer.failures = 1

	if err := h.r.Flush(context.Background()); err == nil {
		t.Fatal("Flush hid a failed write")
	}
	if n := len(h.writer.writes()); n != 1 {
		t.Fatalf("writes after a partial failure = %d, want 1 (the other key still goes out)", n)
	}
	failed := "A"
	if h.writer.keys()[0] == "A" {
		failed = "B"
	}

	h.advance(time.Second)
	h.flush()
	keys := h.writer.keys()
	if len(keys) != 2 || keys[1] != failed {
		t.Errorf("keys written = %v, want the failed one %q retried", keys, failed)
	}

	h.flush()
	if n := len(h.writer.writes()); n != 2 {
		t.Errorf("writes = %d after everything succeeded, want 2", n)
	}
}

func TestFlushWithNothingPendingMakesNoRequest(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.flush()
	if n := len(h.writer.writes()); n != 0 {
		t.Errorf("writes = %d, want 0", n)
	}
}

func TestStartSeedsStateSoARestartWritesNothing(t *testing.T) {
	h := newHarness(t, nil)
	seen := time.Date(2026, 4, 1, 8, 0, 0, 0, time.UTC)
	e := published("Laurin")
	stored, _, err := canonical(Record{Status: e.Status, Reason: e.Reason, Name: e.Name, Lat: e.Lat, Lon: e.Lon, Data: e.Data})
	if err != nil {
		t.Fatal(err)
	}
	stored.FirstSeen, stored.Changed = seen, seen
	h.bridge.docs["P1"] = stored
	// Another schema in the same collection is not ours.
	h.bridge.other = []json.RawMessage{json.RawMessage(
		`{"meta":{"key":"X","schema":"other.v1"},"timestamp":"2026-04-01T08:00:00Z","rawdata":"{}"}`)}

	var onNew []string
	h.r.cfg.OnNew = func(_ context.Context, key string, _ Record) { onNew = append(onNew, key) }
	h.start()

	h.r.Observe("P1", e)
	h.flush()
	if n := len(h.writer.writes()); n != 0 {
		t.Errorf("writes = %d, an unchanged known key must not be rewritten", n)
	}

	h.r.Observe("P1", Entry{Status: StatusWithheld, Name: "Laurin"})
	h.r.Observe("P2", published("new"))
	h.flush()
	if got := h.writer.keys(); !reflect.DeepEqual(got, []string{"P1", "P2"}) {
		t.Fatalf("keys written = %v", got)
	}
	if rec := h.writer.writes()[0].record(t); !rec.FirstSeen.Equal(seen) {
		t.Errorf("FirstSeen = %v, want the seeded %v", rec.FirstSeen, seen)
	}
	if !reflect.DeepEqual(onNew, []string{"P2"}) {
		t.Errorf("OnNew = %v, want only the unknown key", onNew)
	}
}

func TestStartOnAMissingCollectionStartsEmpty(t *testing.T) {
	h := newHarness(t, nil)
	h.bridge.exists = false
	h.start()
	h.r.Observe("P1", published("a"))
	h.flush()
	if n := len(h.writer.writes()); n != 1 {
		t.Errorf("writes = %d, want 1", n)
	}
}

func TestStartValidatesConfig(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"no slash":       func(c *Config) { c.Provider = "famas" },
		"empty db":       func(c *Config) { c.Provider = "/parking-discovery" },
		"empty coll":     func(c *Config) { c.Provider = "famas/" },
		"extra slash":    func(c *Config) { c.Provider = "a/b/c" },
		"empty provider": func(c *Config) { c.Provider = "" },
		"no writer url":  func(c *Config) { c.RawWriterURL = "" },
		"negative gone":  func(c *Config) { c.GoneAfter = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, mod)
			if err := h.r.Start(context.Background()); err == nil {
				t.Error("Start accepted the config")
			}
		})
	}
	if err := New(nil, Config{Provider: testProvider, RawWriterURL: "http://x"}).Start(context.Background()); err == nil {
		t.Error("Start accepted a nil bridge")
	}
}

func TestStartFailsWhenTheBridgeIsBroken(t *testing.T) {
	h := newHarness(t, nil)
	bsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bsrv.Close()
	h.r.bridge = rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: bsrv.URL})
	if err := h.r.Start(context.Background()); err == nil {
		t.Error("Start swallowed a bridge failure: starting empty would rewrite known entries as new")
	}
}

func TestCompleteMarksGoneAfterConsecutiveMisses(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.GoneAfter = 2 })
	h.start()
	h.r.Observe("A", published("a"))
	h.r.Observe("B", published("b"))
	h.r.Complete()
	h.flush()
	h.writer.mu.Lock()
	h.writer.got = nil
	h.writer.mu.Unlock()

	h.advance(time.Hour)
	h.r.Observe("A", published("a"))
	h.r.Complete() // B: miss 1
	h.flush()
	if n := len(h.writer.writes()); n != 0 {
		t.Fatalf("writes after one miss = %d, want 0", n)
	}

	h.r.Observe("A", published("a"))
	h.r.Complete() // B: miss 2 -> gone
	h.flush()
	got := h.writer.writes()
	if len(got) != 1 || got[0].key != "B" {
		t.Fatalf("writes = %v, want only B", h.writer.keys())
	}
	rec := got[0].record(t)
	if rec.Status != StatusGone || rec.Name != "b" || rec.Reason != "no-record" || rec.Data["capacity"] != float64(120) {
		t.Errorf("gone record = %+v, other fields must be kept", rec)
	}
	if !rec.Changed.Equal(*h.clock) {
		t.Errorf("Changed = %v, want %v", rec.Changed, *h.clock)
	}

	// Further snapshots do not rewrite it.
	h.r.Observe("A", published("a"))
	h.r.Complete()
	h.flush()
	if n := len(h.writer.writes()); n != 1 {
		t.Errorf("writes = %d, a gone key must not be rewritten", n)
	}
}

func TestMissCounterResetsWhenObservedAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.GoneAfter = 2 })
	h.start()
	h.r.Observe("B", published("b"))
	h.r.Complete()
	h.r.Complete() // miss 1
	h.r.Observe("B", published("b"))
	h.r.Complete()
	h.r.Complete() // miss 1 again, not 2
	h.flush()
	if rec := h.writer.writes()[0].record(t); rec.Status == StatusGone {
		t.Error("misses were not consecutive but the key went gone")
	}
}

func TestGoneAfterZeroNeverMarksGone(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.r.Observe("B", published("b"))
	for i := 0; i < 5; i++ {
		h.r.Complete()
	}
	h.flush()
	if rec := h.writer.writes()[0].record(t); rec.Status != StatusPublished {
		t.Errorf("status = %s", rec.Status)
	}
}

func TestSeededKeysCountMissesAfterRestart(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.GoneAfter = 1 })
	rec, _, _ := canonical(Record{Status: StatusPublished, Name: "b"})
	rec.FirstSeen, rec.Changed = *h.clock, *h.clock
	h.bridge.docs["B"] = rec
	h.start()

	h.advance(time.Hour)
	h.r.Complete()
	h.flush()
	got := h.writer.writes()
	if len(got) != 1 || got[0].record(t).Status != StatusGone {
		t.Fatalf("a key known only from Start, absent from the snapshot, must go gone: %v", h.writer.keys())
	}
}

func TestGoneKeyComesBackAsAChangeNotNew(t *testing.T) {
	var newCalls int
	h := newHarness(t, func(c *Config) {
		c.GoneAfter = 1
		c.OnNew = func(context.Context, string, Record) { newCalls++ }
	})
	h.start()
	first := *h.clock
	h.r.Observe("B", published("b"))
	h.r.Complete()
	h.r.Complete()
	h.flush()

	h.advance(time.Hour)
	h.r.Observe("B", Entry{Status: StatusWithheld, Reason: "parent-incomplete", Name: "b"})
	h.flush()

	got := h.writer.writes()
	if len(got) != 2 {
		t.Fatalf("writes = %d, want 2 (new, then back)", len(got))
	}
	back := got[1].record(t)
	if back.Status != StatusWithheld || back.Reason != "parent-incomplete" || !back.FirstSeen.Equal(first) {
		t.Errorf("record = %+v", back)
	}
	if newCalls != 1 {
		t.Errorf("OnNew calls = %d, want 1", newCalls)
	}
}

func TestInvalidEntriesAreIgnored(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.r.Observe("", published("a"))
	h.r.Observe("A", Entry{Name: "no status"})
	h.r.Observe("B", Entry{Status: StatusGone})
	h.r.Observe("C", Entry{Status: StatusPublished, Data: map[string]any{"f": func() {}}})
	h.flush()
	if n := len(h.writer.writes()); n != 0 {
		t.Errorf("writes = %d, want 0", n)
	}
}

func TestObserveDoesNotAliasTheCallersMaps(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	data := map[string]any{"capacity": 10}
	h.r.Observe("A", Entry{Status: StatusPublished, Data: data})
	data["capacity"] = 99
	h.flush()
	if got := h.writer.writes()[0].record(t).Data["capacity"]; got != float64(10) {
		t.Errorf("capacity = %v, the caller's later mutation leaked in", got)
	}
}

func TestPanickingOnNewDoesNotEscape(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.OnNew = func(context.Context, string, Record) { panic("boom") }
	})
	h.start()
	h.r.Observe("A", published("a"))
	h.flush()
	if n := len(h.writer.writes()); n != 1 {
		t.Errorf("writes = %d, want 1", n)
	}
}

func TestConcurrentObserveFlushComplete(t *testing.T) {
	var mu sync.Mutex
	newKeys := map[string]int{}
	h := newHarness(t, func(c *Config) {
		c.GoneAfter = 3
		c.OnNew = func(_ context.Context, key string, _ Record) {
			mu.Lock()
			newKeys[key]++
			mu.Unlock()
		}
	})
	h.start()
	// The clock is read from many goroutines.
	h.r.now = func() time.Time { return time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC) }

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				key := fmt.Sprintf("K%02d", i%10)
				h.r.Observe(key, published(fmt.Sprintf("name-%d", (g+i)%3)))
				if i%10 == 0 {
					_ = h.r.Flush(context.Background())
					h.r.Complete()
				}
			}
		}(g)
	}
	wg.Wait()
	h.flush()

	if len(newKeys) != 10 {
		t.Errorf("new keys = %d, want 10", len(newKeys))
	}
	for k, n := range newKeys {
		if n != 1 {
			t.Errorf("OnNew(%s) called %d times", k, n)
		}
	}
	h.flush()
	before := len(h.writer.writes())
	h.flush()
	if after := len(h.writer.writes()); after != before {
		t.Errorf("a settled reporter wrote %d more documents", after-before)
	}
}

func TestLargeIntegersSurviveExactly(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.r.Observe("A", Entry{Status: StatusPublished, Data: map[string]any{"id": int64(9007199254740993)}})
	h.flush()
	if body := string(h.writer.writes()[0].body); !strings.Contains(body, `"id":9007199254740993`) {
		t.Errorf("body = %s, the integer was rounded", body)
	}
}
