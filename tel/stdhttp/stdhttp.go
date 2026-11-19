// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

// Package stdhttp instruments net/http servers and clients with the
// telemetry of package tel: a server span per request named after the
// ServeMux route, a logger carrying the trace id, the request metrics, an
// access-log line, panic recovery, and a client transport that opens a span
// per outgoing call and propagates the trace. It depends on nothing beyond
// the standard library and OpenTelemetry, unlike tel/http, which brings gin.
package stdhttp

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Options tune Middleware. The zero value observes every request, logs
// every request at INFO and answers a panic with a plain-text 500.
type Options struct {
	// Skip marks requests that get only the panic guard: health probes,
	// static files.
	Skip func(r *http.Request) bool
	// Level picks the access-log level of a finished request, for instance
	// DEBUG for the successes of a high-volume route. Nil means INFO.
	Level func(r *http.Request, status int) slog.Level
	// OnPanic writes the response to a recovered panic when the handler had
	// not started writing. Nil writes a text/plain 500.
	OnPanic func(w http.ResponseWriter, r *http.Request)
}

// Middleware wraps a handler, normally the ServeMux, so that the route it
// matched is known when the request finishes.
func Middleware(opts Options) func(http.Handler) http.Handler {
	duration, _ := tel.MeterInt64Histogram(tel.MetricRequestDurationMillis)
	inflight, _ := tel.MeterInt64UpDownCounter(tel.MetricRequestsInFlight)
	if opts.OnPanic == nil {
		opts.OnPanic = func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w}
			if opts.Skip != nil && opts.Skip(r) {
				defer func() { handlePanic(recover(), sw, r, opts.OnPanic) }()
				next.ServeHTTP(sw, r)
				return
			}

			start := time.Now()
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := tel.TraceStart(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.request.method", r.Method),
					attribute.String("url.path", r.URL.Path),
				))
			ctx = logger.WithTracedLogger(ctx)
			r = r.WithContext(ctx)
			inflight.Add(ctx, 1)

			defer func() {
				handlePanic(recover(), sw, r, opts.OnPanic)
				inflight.Add(ctx, -1)

				// ServeMux fills r.Pattern on this request while routing.
				route := r.Pattern
				if route == "" {
					route = "unmatched"
				}
				attrs := []attribute.KeyValue{
					attribute.String("http.route", route),
					attribute.String("http.request.method", r.Method),
					attribute.Int("http.response.status_code", sw.status),
				}
				span.SetName(route)
				span.SetAttributes(attrs...)
				if sw.status >= http.StatusInternalServerError {
					span.SetStatus(codes.Error, http.StatusText(sw.status))
				}
				span.End()

				ms := time.Since(start).Milliseconds()
				duration.Record(ctx, ms, metric.WithAttributes(attrs...))

				level := slog.LevelInfo
				if opts.Level != nil {
					level = opts.Level(r, sw.status)
				}
				logger.Get(ctx).Log(ctx, level, "request",
					"method", r.Method, "path", r.URL.Path, "route", route,
					"status", sw.status, "duration_ms", ms, "client", ClientIP(r))
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// statusWriter remembers what was sent, for the access log and so a panic
// after the handler started writing is not answered twice.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the original writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// handlePanic takes the value of a recover() called in the deferred function
// itself; recover() one frame deeper finds nothing. http.ErrAbortHandler
// keeps its meaning.
func handlePanic(p any, w *statusWriter, r *http.Request, onPanic func(http.ResponseWriter, *http.Request)) {
	if p == nil {
		return
	}
	if p == http.ErrAbortHandler {
		panic(p)
	}
	err := fmt.Errorf("panic: %v", p)
	logger.Get(r.Context()).Error("handler panicked", "method", r.Method, "path", r.URL.Path,
		"err", err, "stack", string(debug.Stack()))
	span := trace.SpanFromContext(r.Context())
	span.RecordError(err)
	span.SetStatus(codes.Error, "panic")
	if w.status == 0 {
		onPanic(w, r)
	}
}

// ClientIP is the first address in X-Forwarded-For, or the peer when the
// request did not come through a proxy.
func ClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		first, _, _ := strings.Cut(fwd, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Transport wraps an outgoing client with a client span per call and the
// W3C trace headers, so a slow dependency shows up under the request that
// waited for it. Query strings and headers are never recorded.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base}
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := tel.TraceStart(req.Context(), req.Method+" "+req.URL.Host,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", req.Method),
			attribute.String("server.address", req.URL.Host),
			attribute.String("url.path", req.URL.Path),
		))
	defer span.End()
	req = req.Clone(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, resp.Status)
	}
	return resp, nil
}
