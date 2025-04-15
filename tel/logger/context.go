// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package logger

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type contextKey string

const loggerKey contextKey = "opendatahub.tel.loggger"

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

func WithTracedLogger(ctx context.Context) context.Context {
	sc := trace.SpanContextFromContext(ctx)

	if sc.IsValid() {
		l := slog.Default().With(
			"trace_id", sc.TraceID().String(),
			"span_id", sc.SpanID().String(),
		)
		// Store logger in context
		return context.WithValue(ctx, loggerKey, l)
	}
	return context.WithValue(ctx, loggerKey, slog.Default())
}

func Get(ctx context.Context) *slog.Logger {
	l, ok := ctx.Value(loggerKey).(*slog.Logger)
	if !ok || l == nil {
		return slog.Default()
	}
	return l
}
