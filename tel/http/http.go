// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package http

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TracingRoundTripper is a custom RoundTripper that injects trace context.
type TracingRoundTripper struct {
	Base http.RoundTripper
}

func (trt *TracingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Ensure a base transport is set.
	if trt.Base == nil {
		trt.Base = http.DefaultTransport
	}
	// Inject tracing context into HTTP headers.
	propagator := otel.GetTextMapPropagator()
	propagator.Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	return trt.Base.RoundTrip(req)
}

// NewHttpTelClient creates an HTTP client that injects trace context.
func NewHttpTelClient() *http.Client {
	return &http.Client{
		Transport: &TracingRoundTripper{},
	}
}

// contextKey is used to store/retrieve the HTTP client from context.
type contextKey string

const httpClientKey contextKey = "opendatahub.tel.http-client"

// WithHTTP returns a new context with the provided HTTP client.
func WithHTTP(ctx context.Context, client *http.Client) context.Context {
	return context.WithValue(ctx, httpClientKey, client)
}

// GetHTTP retrieves the HTTP client from the context,
// or returns http.DefaultClient if none is set.
func GetHTTP(ctx context.Context) *http.Client {
	if client, ok := ctx.Value(httpClientKey).(*http.Client); ok && client != nil {
		return client
	}
	return http.DefaultClient
}
