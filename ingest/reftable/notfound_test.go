// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
)

// A collection that does not exist used to answer 200 with an empty page, which
// is indistinguishable from a collection nothing has been written to yet. The
// table then bootstrapped empty and reported success, and the consumer published
// defaults over every value an operator had set.
func TestBootstrapFailsWhenTheCollectionDoesNotExist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such collection"}`))
	}))
	defer srv.Close()

	tbl := New[map[string]any](
		rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: srv.URL}),
		Config{Name: "enrichment", DB: "enrichment", Collection: "typo", Key: "key"})

	err := tbl.bootstrap(context.Background())
	if err == nil {
		t.Fatal("bootstrap succeeded against a collection that does not exist")
	}
	if !errors.Is(err, rdb.ErrCollectionNotFound) {
		t.Errorf("err = %v, want it to wrap ErrCollectionNotFound", err)
	}
	// The message has to name what to go and check, or an operator sees only
	// that a transformer will not start.
	for _, want := range []string{"enrichment", "typo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// An empty collection that does exist is a legitimate cold start, and must not
// stop the process.
func TestBootstrapAcceptsAnEmptyCollection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"count":0,"data":[],"field":"key","next":""}`))
	}))
	defer srv.Close()

	tbl := New[map[string]any](
		rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: srv.URL}),
		Config{Name: "enrichment", DB: "enrichment", Collection: "parking", Key: "key"})

	if err := tbl.bootstrap(context.Background()); err != nil {
		t.Fatalf("an existing empty collection must bootstrap cleanly, got %v", err)
	}
	if tbl.Len() != 0 {
		t.Errorf("keys = %d, want 0", tbl.Len())
	}
}

// A consumer that publishes unchanged without records may start before the
// first record exists, and picks it up once the collection appears.
func TestAllowMissingReadsAMissingCollectionAsEmpty(t *testing.T) {
	exists := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"field":"key","next":"","data":[` +
			`{"meta":{"key":"a"},"rawdata":"{\"v\":1}","bsontimestamp":"2026-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	tbl := New[map[string]any](
		rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: srv.URL}),
		Config{Name: "enrichment", DB: "enrichment", Collection: "new", Key: "key", AllowMissing: true})

	if err := tbl.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap with AllowMissing must accept a missing collection, got %v", err)
	}
	if tbl.Len() != 0 {
		t.Errorf("keys = %d, want 0", tbl.Len())
	}

	exists = true
	if _, err := tbl.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := tbl.Get("a"); !ok {
		t.Error("a record published after start was not picked up")
	}
}
