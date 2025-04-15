// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package tel

import (
	"context"

	otelmetric "go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func Enabled() bool {
	return GetTelemetry().Enabled()
}

// GetServiceName returns the name of the service.
func GetServiceName() string {
	return GetTelemetry().GetServiceName()
}

// MeterInt64Histogram creates a new int64 histogram metric.
func MeterInt64Histogram(metric Metric) (otelmetric.Int64Histogram, error) {
	return GetTelemetry().MeterInt64Histogram(metric)
}

// MeterInt64UpDownCounter creates a new int64 up down counter metric.
func MeterInt64UpDownCounter(metric Metric) (otelmetric.Int64UpDownCounter, error) {
	return GetTelemetry().MeterInt64UpDownCounter(metric)
}

// TraceStart starts a new span with the given name. The span must be ended by calling End.
func TraceStart(ctx context.Context, name string, opts ...oteltrace.SpanStartOption) (context.Context, oteltrace.Span) {
	return GetTelemetry().TraceStart(ctx, name, opts...)
}

// Shutdown shuts down the logger, meter, and tracer.
func Shutdown(ctx context.Context) {
	GetTelemetry().Shutdown(ctx)
}

func FlushOnPanic() {
	GetTelemetry().FlushOnPanic()
}
