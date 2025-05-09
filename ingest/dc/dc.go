// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package dc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/ms"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/qmill"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
)

type Env struct {
	PROVIDER    string
	MQ_URI      string
	MQ_EXCHANGE string `default:"ingress"`
	MQ_CLIENT   string
}

func PubFromEnv(ctx context.Context, e Env) (*qmill.QMill, error) {
	return qmill.NewPublisherQmill(ctx, e.MQ_URI, e.MQ_CLIENT,
		qmill.WithExchange(e.MQ_EXCHANGE, "fanout", false),
		qmill.WithNoRequeueOnNack(true),
		qmill.WithLogger(watermill.NewSlogLogger(slog.Default())),
	)
}

type Handler[P any] func(context.Context, P) (*rdb.RawAny, error)
type Input[P any] struct {
	data P
	ctx  context.Context
}
type EmptyData *any

func NewInput[P any](ctx context.Context, data P) Input[P] {
	return Input[P]{
		data: data,
		ctx:  ctx,
	}
}

type Dc[P any] struct {
	config Env
	pub    *qmill.QMill
	input  chan Input[P]
}

func NewDc[P any](ctx context.Context, config Env) *Dc[P] {
	pub, err := PubFromEnv(ctx, config)
	if err != nil {
		logger.Get(ctx).Error("failed to initialize Dc pub", "err", err)
		panic(err)
	}
	return &Dc[P]{
		config: config,
		pub:    pub,
		input:  make(chan Input[P]),
	}
}

func (d *Dc[P]) GetInputChannel() chan<- Input[P] {
	return d.input
}

func (d *Dc[P]) Start(ctx context.Context, handler Handler[P]) error {
	defer close(d.input)
	for data := range d.input {
		jobstart := time.Now()

		ctx, collection := d.StartCollection(data.ctx)

		log := logger.Get(ctx)
		log.Debug("collecting")

		raw_data, err := handler(ctx, data.data)
		err = collection.Publish(ctx, raw_data)
		ms.FailOnError(ctx, err, "failed to publish raw payload", "payload", fmt.Sprintf("%v", raw_data))

		log.Info("collection completed", "runtime_ms", time.Since(jobstart).Milliseconds())

		// end collection
		collection.End(ctx)
	}

	err := errors.New("DC unexpected input channel close")
	slog.Error(err.Error())
	return err
}

func (d *Dc[P]) StartCollection(ctx context.Context) (context.Context, *Collection) {
	return NewCollection(ctx, d.pub)
}
