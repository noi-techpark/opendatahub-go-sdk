// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package ms

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/kelseyhightower/envconfig"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/trace"
)

func InitWithEnv(ctx context.Context, env_prefix string, env interface{}) {
	logger.InitLogging()
	envconfig.MustProcess(env_prefix, env)

	tel.NewTelemetryFromEnv(context.Background())

	if tel.Enabled() {
		GracefullShutdowner(ctx, tel.Shutdown)
	}
}

// failOnError records error info on the span, flushes pending spans, and panics.
func FailOnError(ctx context.Context, err error, msg string, args ...any) {
	if err == nil {
		return
	}

	tel.OnError(ctx, msg, err)
	trace.SpanFromContext(ctx).End()

	args_ := append([]any{"err", err}, args...)
	logger.Get(ctx).Error(msg, args_...)
	panic(err)
}

type Finalizer func(context.Context)

func GracefullShutdowner(ctx context.Context, finalizers ...Finalizer) {
	// gracefull shutdown
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
		<-sigs // Wait for termination signal

		for _, fin := range finalizers {
			fin(ctx)
		}
		os.Exit(0)
	}()
}
