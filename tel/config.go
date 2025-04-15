// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package tel

import "github.com/kelseyhightower/envconfig"

// Config holds the configuration for the telemetry.
type Config struct {
	ServiceName    string `envconfig:"SERVICE_NAME"      default:"gotel"`
	ServiceVersion string `envconfig:"SERVICE_VERSION"   default:"0.0.1"`

	Enabled bool `envconfig:"TELEMETRY_ENABLED" default:"true"`

	TraceEnable               bool   `envconfig:"TELEMETRY_TRACE_ENABLED"   default:"true"`
	TraceBatchSize            int    `envconfig:"TELEMETRY_TRACE_BATCH_SIZE"   default:"10"`
	TraceBatchTimeoutSec      int    `envconfig:"TELEMETRY_TRACE_BATCH_TIMEOUT_SEC"   default:"5"`
	TraceExporterGrpcEndpoint string `envconfig:"TELEMETRY_TRACE_GRPC_ENDPOINT"   default:"localhost:4317"`

	TraceExporterTLSEnabled bool   `envconfig:"TELEMETRY_TRACE_TLS_ENABLED" default:"false"`
	TraceExporterTLSCert    string `envconfig:"TELEMETRY_TRACE_TLS_CERT" default:""` // Path to certificate file

	MetricsEnable               bool   `envconfig:"TELEMETRY_METRICS_ENABLED"   default:"false"`
	MetricsTimeoutSec           int    `envconfig:"TELEMETRY_METRICS_TIMEOUT_SEC"   default:"30"`
	MetricsIntervalSec          int    `envconfig:"TELEMETRY_METRICS_INTERVAL_SEC"   default:"60"`
	MetricsExporterGrpcEndpoint string `envconfig:"TELEMETRY_METRICS_GRPC_ENDPOINT"   default:"localhost:4317"`

	MetricsExporterTLSEnabled bool   `envconfig:"TELEMETRY_METRICS_TLS_ENABLED" default:"false"`
	MetricsExporterTLSCert    string `envconfig:"TELEMETRY_METRICS_TLS_CERT" default:""` // Path to certificate file

	MetricsExportMode   string `envconfig:"TELEMETRY_METRICS_EXPORT_MODE" default:"otlp"`
	MetricsExporterPort int    `envconfig:"TELEMETRY_METRICS_EXPORT_PORT" default:"2112"`
}

// NewConfigFromEnv creates a new telemetry config from the environment.
func NewConfigFromEnv() Config {
	telem := Config{}
	envconfig.MustProcess("", &telem)
	return telem
}
