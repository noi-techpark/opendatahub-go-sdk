// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package dc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/qmill"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/trace"
)

type Collection struct {
	spans []*trace.Span
	pub   *qmill.QMill
}

func NewCollection(ctx context.Context, pub *qmill.QMill) (context.Context, *Collection) {
	c := &Collection{
		spans: make([]*trace.Span, 0),
		pub:   pub,
	}

	// check if the provided context is already recording
	root_span_is_recording := trace.SpanFromContext(ctx).IsRecording()
	if !root_span_is_recording {
		ctx_, serverSpan, collectorSpan := initializeSpans(ctx)
		c.spans = append(c.spans, serverSpan, collectorSpan)
		ctx = ctx_
	}

	// logger uses root span if recording, otherwhise collectorSpan
	ctx = logger.WithTracedLogger(ctx)
	return ctx, c
}

func (c *Collection) Publish(ctx context.Context, raw_data *rdb.RawAny) error {
	payload, err := json.Marshal(raw_data)
	if err != nil {
		return fmt.Errorf("failed to marshal raw_data: %s", err.Error())
	}

	return c.pub.Publish(ctx, payload, "")
}

func (c *Collection) End(ctx context.Context) {
	tel.OnSuccess(ctx)

	// end all spans
	for _, s := range c.spans {
		(*s).End()
	}
}

func initializeSpans(ctx context.Context) (context.Context, *trace.Span, *trace.Span) {
	// root server span to enable RED collection of the collector span
	ctx, serverSpan := tel.TraceStart(
		ctx,
		fmt.Sprintf("%s.trigger", tel.GetServiceName()),
		trace.WithSpanKind(trace.SpanKindServer),
	)

	// collect span creation
	ctx, producerSpan := tel.TraceStart(
		ctx,
		fmt.Sprintf("%s.collect", tel.GetServiceName()),
		trace.WithSpanKind(trace.SpanKindProducer),
	)

	return ctx, &serverSpan, &producerSpan
}
