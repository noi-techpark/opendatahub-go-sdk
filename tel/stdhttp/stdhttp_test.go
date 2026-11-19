// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package stdhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareTurnsPanicInto500(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	rec := httptest.NewRecorder()
	Middleware(Options{})(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
}

func TestMiddlewareUsesTheCallersPanicResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	opts := Options{OnPanic: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error"}`))
	}}
	rec := httptest.NewRecorder()
	Middleware(opts)(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if !strings.Contains(rec.Body.String(), `"internal_error"`) {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestMiddlewareLeavesAWrittenResponseAlone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /half", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("after the header")
	})
	rec := httptest.NewRecorder()
	Middleware(Options{})(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/half", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want the handler's 202", rec.Code)
	}
}

func TestMiddlewareGuardsSkippedRequestsToo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) { panic("boom") })
	opts := Options{Skip: func(r *http.Request) bool { return r.URL.Path == "/healthz" }}
	rec := httptest.NewRecorder()
	Middleware(opts)(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
}

func TestMiddlewareSeesTheRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		seen = r.Pattern
	})
	Middleware(Options{})(probe).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/42", nil))
	if seen != "GET /items/{id}" {
		t.Fatalf("pattern %q", seen)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.9:4321"
	if got := ClientIP(r); got != "10.0.0.9" {
		t.Fatalf("peer: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("forwarded: %q", got)
	}
}

func TestTransportReachesTheBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	defer srv.Close()
	resp, err := (&http.Client{Transport: Transport(nil)}).Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
