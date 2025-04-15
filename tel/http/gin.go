// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TracingMiddleware checks for OT headers (e.g. "traceparent") and starts a new span.
func TracingMiddleware(excluded_paths []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !tel.Enabled() {
			c.Next()
			return
		}

		for _, path := range excluded_paths {
			if c.Request.URL.Path == path {
				c.Next()
				return
			}
		}

		// Extract trace context from incoming HTTP headers.
		propagator := otel.GetTextMapPropagator()
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))

		path := c.FullPath()
		if path == "" {
			// fallback to raw path if FullPath isn't available (e.g., unmatched route)
			path = c.Request.URL.Path
		}

		// Start a new span for the incoming request.
		ctx, span := tel.TraceStart(
			ctx,
			fmt.Sprintf("%s %s %s", tel.GetServiceName(), c.Request.Method, path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("url", c.Request.URL.Path),
			),
		)

		// logger
		ctx = logger.WithTracedLogger(ctx)

		// Replace the request context with the new one containing the span.
		c.Request = c.Request.WithContext(ctx)

		// Process the request.
		c.Next()

		// End the span once processing is complete.
		span.End()
	}
}
