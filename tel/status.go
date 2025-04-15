// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package tel

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/codes"
)

// OnError gets the logger and span from cotext, logs the error and set span status to error.
// The span is immediately ended
func OnError(ctx context.Context, msg string, err error) {
	log := logger.Get(ctx)
	log.Error(msg, "err", err)

	span := trace.SpanFromContext(ctx)

	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func OnSuccess(ctx context.Context) {
	span := trace.SpanFromContext(ctx)
	span.SetStatus(codes.Ok, "success")
}
