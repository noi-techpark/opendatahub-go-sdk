// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package tel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc" // For reference only
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"google.golang.org/grpc/credentials"
)

// createTLSCredentials loads your certificate and creates grpc.TransportCredentials.
func createTLSCredentials(certFile string) (credentials.TransportCredentials, error) {
	certData, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read certificate file: %w", err)
	}
	cp := x509.NewCertPool()
	if ok := cp.AppendCertsFromPEM(certData); !ok {
		return nil, fmt.Errorf("failed to append certificate")
	}

	// Construct credentials; optionally, set other tls.Config options.
	tlsConfig := &tls.Config{
		RootCAs: cp,
	}
	return credentials.NewTLS(tlsConfig), nil
}

// newLoggerProvider creates a new logger provider with the OTLP gRPC exporter.
func newLoggerProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*log.LoggerProvider, error) {
	exporter, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	processor := log.NewBatchProcessor(exporter)
	lp := log.NewLoggerProvider(
		log.WithProcessor(processor),
		log.WithResource(res),
	)

	return lp, nil
}

// newMeterProvider decides which meter provider to create (OTLP or Prometheus).
func newMeterProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*metric.MeterProvider, error) {
	switch cfg.MetricsExportMode {
	case "prometheus":
		return newPrometheusMeterProvider(ctx, cfg, res)
	case "otlp":
		return newOTLPMeterProvider(ctx, cfg, res)
	default:
		return nil, fmt.Errorf("unsupported metrics export mode: %q", cfg.MetricsExportMode)
	}
}

// newOTLPMeterProvider configures a push-based meter via OTLP.
func newOTLPMeterProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*metric.MeterProvider, error) {
	// Build OTLP metric exporter options
	var opts []otlpmetricgrpc.Option
	if cfg.MetricsExporterTLSEnabled {
		creds, err := createTLSCredentials(cfg.MetricsExporterTLSCert)
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS credentials for OTLP metric exporter: %w", err)
		}
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(creds))
	} else {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.MetricsExporterGrpcEndpoint))

	// Create the OTLP exporter
	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP metric exporter: %w", err)
	}

	// Configure a PeriodicReader with user-defined batch interval.
	// This controls how often we push metrics to the endpoint.
	periodicReader := metric.NewPeriodicReader(exporter,
		metric.WithInterval(time.Duration(cfg.MetricsIntervalSec)*time.Second),
		metric.WithTimeout(time.Duration(cfg.MetricsTimeoutSec)*time.Second),
	)

	mp := metric.NewMeterProvider(
		metric.WithReader(periodicReader),
		metric.WithResource(res),
	)

	// Make this the global MeterProvider
	otel.SetMeterProvider(mp)
	return mp, nil
}

// newPrometheusMeterProvider configures a pull-based meter for Prometheus.
func newPrometheusMeterProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	// Create a Prometheus exporter. Various options exist (e.g., WithNamespace).
	exp, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("creating prometheus exporter: %w", err)
	}

	// The Prometheus exporter *is* a "Reader" in OTel metrics terminology,
	// so you attach it to the MeterProvider with `WithReader`.
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(exp),
	)

	// A typical usage is to return `exp` as an http.Handler, so you can register it on a route.
	// http.Handle("/metrics", promhttp.Handler())
	// go http.ListenAndServe(":2112", nil)

	http.Handle("/metrics", promhttp.Handler())
	go http.ListenAndServe(fmt.Sprintf(":%d", cfg.MetricsExporterPort), nil)

	return mp, nil
}

// newTracerProvider creates a new tracer provider with the OTLP gRPC exporter.
func newTracerProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*trace.TracerProvider, error) {
	opts := make([]otlptracegrpc.Option, 0)

	if cfg.TraceExporterTLSEnabled {
		creds, err := createTLSCredentials(cfg.TraceExporterTLSCert)
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS credentials for OTLP trace exporter: %w", err)
		}
		opts = append(opts, otlptracegrpc.WithTLSCredentials(creds))
	} else {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	opts = append(opts, otlptracegrpc.WithEndpoint(cfg.TraceExporterGrpcEndpoint))

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// Create Resource
	tp := trace.NewTracerProvider(
		trace.WithBatcher(exporter,
			trace.WithBatchTimeout(time.Duration(cfg.TraceBatchTimeoutSec)*time.Second),
			trace.WithMaxExportBatchSize(cfg.TraceBatchSize),
		),
		trace.WithResource(res),
	)

	return tp, nil
}

// newResource creates a new OTEL resource with the service name and version.
func newResource(serviceName string, serviceVersion string) *resource.Resource {
	hostName, _ := os.Hostname()

	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(serviceVersion),
		semconv.HostName(hostName),
	)
}
