// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package dc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The writer canonicalises header names before matching its prefix, so what a
// collector sets is not byte-for-byte what the writer sees. These tests pin the
// round trip, because a key that changes shape in flight becomes a reference
// table that is quietly missing rows.
func TestSendRawEmitsMetaHeaders(t *testing.T) {
	var got http.Header
	var body []byte
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ts := time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC)
	err := sendRaw(srv.URL, "skidata/counting-categories", ts, []byte(`{"a":1}`), "application/json",
		map[string]string{"facility": "0607242"})
	if err != nil {
		t.Fatalf("sendRaw: %v", err)
	}

	if path != "/skidata/counting-categories/2026-08-12T10:30:00Z" {
		t.Errorf("path = %q", path)
	}
	if string(body) != `{"a":1}` {
		t.Errorf("body = %q", body)
	}
	// http.Header.Get is canonicalisation-insensitive, which is exactly how the
	// writer reads it back.
	if v := got.Get(MetaHeaderPrefix + "facility"); v != "0607242" {
		t.Errorf("meta header = %q, want 0607242", v)
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", got.Get("Content-Type"))
	}
}

// The writer lowercases the part after the prefix, so "Facility" and "facility"
// land on the same document field. Sending mixed case must not produce a
// different key than the table later looks up.
func TestSendRawMetaKeyCaseIsIrrelevant(t *testing.T) {
	for _, key := range []string{"facility", "Facility", "FACILITY"} {
		var got http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))

		if err := sendRaw(srv.URL, "a/b", time.Now(), []byte("{}"), "application/json",
			map[string]string{key: "x"}); err != nil {
			srv.Close()
			t.Fatalf("sendRaw(%s): %v", key, err)
		}
		srv.Close()

		// Whatever case went in, exactly one prefixed header comes out and it
		// resolves under the canonical name.
		n := 0
		for k := range got {
			if strings.HasPrefix(strings.ToLower(k), strings.ToLower(MetaHeaderPrefix)) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("key %q produced %d meta headers, want 1", key, n)
		}
		if v := got.Get(MetaHeaderPrefix + "facility"); v != "x" {
			t.Errorf("key %q: lookup returned %q, want x", key, v)
		}
	}
}

func TestSendRawRejectsUnusableMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cases := map[string]map[string]string{
		"empty key":      {"": "v"},
		"key with space": {"my key": "v"},
		"key with dot":   {"meta.facility": "v"},
		"key with colon": {"fac:ility": "v"},
		"value with LF":  {"facility": "a\nb"},
		"value with CR":  {"facility": "a\rb"},
	}

	for name, meta := range cases {
		if err := sendRaw(srv.URL, "a/b", time.Now(), []byte("{}"), "application/json", meta); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

func TestSendRawWithoutMetaIsUnchanged(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := sendRaw(srv.URL, "a/b", time.Now(), []byte("{}"), "application/json", nil); err != nil {
		t.Fatalf("sendRaw: %v", err)
	}
	for k := range got {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(MetaHeaderPrefix)) {
			t.Errorf("unexpected meta header %q on a publish with no meta", k)
		}
	}
}

func TestSendRawSurfacesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := sendRaw(srv.URL, "a/b", time.Now(), []byte("{}"), "application/json", nil); err == nil {
		t.Error("expected an error for a 400 response")
	}
}
