// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package tel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var noopProvider *noopTelemetry = newNoopTelemetry()
var globalTelemetry TelemetryProvider = noopProvider

// TelemetryProvider is an interface for the telemetry provider.
type TelemetryProvider interface {
	Enabled() bool
	GetServiceName() string
	MeterInt64Histogram(metric Metric) (otelmetric.Int64Histogram, error)
	MeterInt64UpDownCounter(metric Metric) (otelmetric.Int64UpDownCounter, error)
	TraceStart(ctx context.Context, name string, opts ...oteltrace.SpanStartOption) (context.Context, oteltrace.Span)
	Shutdown(ctx context.Context)
	FlushOnPanic()
}

// Telemetry is a wrapper around the OpenTelemetry logger, meter, and tracer.
type Telemetry struct {
	mp     *metric.MeterProvider
	tp     *trace.TracerProvider
	meter  otelmetric.Meter
	tracer oteltrace.Tracer
	cfg    Config
}

// NewTelemetry creates a new telemetry instance.
func NewTelemetry(ctx context.Context, cfg Config) (TelemetryProvider, error) {
	tel := &Telemetry{
		cfg: cfg,
	}
	// initialize meter and tracer with noop provider
	tel.meter = noopProvider.mp.Meter("")
	tel.tracer = noopProvider.tp.Tracer("")

	if !cfg.Enabled {
		return tel, nil
	}

	rp := newResource(cfg.ServiceName, cfg.ServiceVersion)

	if cfg.TraceEnable {
		tp, err := newTracerProvider(ctx, &cfg, rp)
		if err != nil {
			return nil, fmt.Errorf("failed to create tracer: %w", err)
		}
		tel.tracer = tp.Tracer(cfg.ServiceName)

		// GLOBAL propagator
		// Set the global propagator to include both TraceContext and Baggage.
		otel.SetTextMapPropagator(
			propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{},
				propagation.Baggage{},
			),
		)
		otel.SetTracerProvider(tp)
		slog.Info(fmt.Sprintf("started telemetry trace. exporter set to %s", cfg.TraceExporterGrpcEndpoint))
	}

	if cfg.MetricsEnable {
		mp, err := newMeterProvider(ctx, &cfg, rp)
		if err != nil {
			return nil, fmt.Errorf("failed to create tracer: %w", err)
		}
		tel.meter = mp.Meter(cfg.ServiceName)
		otel.SetMeterProvider(mp)
		slog.Info(fmt.Sprintf("started telemetry meter with mode %s", cfg.MetricsExportMode))
	}

	return tel, nil
}

// NewTelemetryFromEnv returns a telemetry object using env variables and sets it as global tel
func NewTelemetryFromEnv(ctx context.Context) TelemetryProvider {
	config := NewConfigFromEnv()
	tel, err := NewTelemetry(ctx, config)
	if err != nil {
		logger.Get(ctx).Error("failed to initialize Tel", "err", err)
		panic(err)
	}
	SetTelemetry(tel)
	return tel
}

func SetTelemetry(tel TelemetryProvider) {
	globalTelemetry = tel
}

func GetTelemetry() TelemetryProvider {
	return globalTelemetry
}

func (t *Telemetry) Enabled() bool {
	return t.cfg.Enabled
}

// GetServiceName returns the name of the service.
func (t *Telemetry) GetServiceName() string {
	return t.cfg.ServiceName
}

// MeterInt64Histogram creates a new int64 histogram metric.
func (t *Telemetry) MeterInt64Histogram(metric Metric) (otelmetric.Int64Histogram, error) { //nolint:ireturn
	histogram, err := t.meter.Int64Histogram(
		metric.Name,
		otelmetric.WithDescription(metric.Description),
		otelmetric.WithUnit(metric.Unit),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create histogram: %w", err)
	}

	return histogram, nil
}

// MeterInt64UpDownCounter creates a new int64 up down counter metric.
func (t *Telemetry) MeterInt64UpDownCounter(metric Metric) (otelmetric.Int64UpDownCounter, error) { //nolint:ireturn
	counter, err := t.meter.Int64UpDownCounter(
		metric.Name,
		otelmetric.WithDescription(metric.Description),
		otelmetric.WithUnit(metric.Unit),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create counter: %w", err)
	}

	return counter, nil
}

// TraceStart starts a new span with the given name. The span must be ended by calling End.
func (t *Telemetry) TraceStart(ctx context.Context, name string, opts ...oteltrace.SpanStartOption) (context.Context, oteltrace.Span) { //nolint:ireturn
	//nolint: spancheck
	return t.tracer.Start(ctx, name, opts...)
}

// Shutdown shuts down the logger, meter, and tracer.
func (t *Telemetry) Shutdown(ctx context.Context) {
	logger.Get(ctx).Info("Flushing telemetry...")

	if t.mp != nil {
		t.mp.ForceFlush(ctx)
		t.mp.Shutdown(ctx)
	}

	if t.tp != nil {
		t.tp.ForceFlush(ctx)
		t.tp.Shutdown(ctx)
	}
}

func (t *Telemetry) FlushOnPanic() {
	if r := recover(); r != nil {
		fmt.Printf("panic: %v\n%s\n", r, debug.Stack())

		// Force flush spans with a timeout to ensure telemetry is sent.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		t.Shutdown(ctx)

		// Exit with non-zero code instead of re-panicking
		os.Exit(1)
	}
}
