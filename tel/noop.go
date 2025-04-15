// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package tel

import (
	"context"

	otelmetric "go.opentelemetry.io/otel/metric"
	otelnoopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	otelnooptrace "go.opentelemetry.io/otel/trace/noop"
)

type noopTelemetry struct {
	tp otelnooptrace.TracerProvider
	mp otelnoopmetric.MeterProvider
}

// newNoopTelemetry returns a TelemetryProvider that performs no operations.
func newNoopTelemetry() *noopTelemetry {
	return &noopTelemetry{
		tp: otelnooptrace.NewTracerProvider(),
		mp: otelnoopmetric.NewMeterProvider(),
	}
}

func (n *noopTelemetry) Enabled() bool {
	return false
}

func (n *noopTelemetry) GetServiceName() string {
	return "noop"
}

func (n *noopTelemetry) MeterInt64Histogram(metric Metric) (otelmetric.Int64Histogram, error) {
	return n.mp.Meter("").Int64Histogram(metric.Name)
}

func (n *noopTelemetry) MeterInt64UpDownCounter(metric Metric) (otelmetric.Int64UpDownCounter, error) {
	return n.mp.Meter("").Int64UpDownCounter(metric.Name)
}

func (n *noopTelemetry) TraceStart(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return n.tp.Tracer("").Start(ctx, name, opts...)
}

func (n *noopTelemetry) Shutdown(ctx context.Context) {
	// no-op
}

func (n *noopTelemetry) FlushOnPanic() {
	// no-op
}
