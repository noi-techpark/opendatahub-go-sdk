// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package tr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/urn"
	"github.com/noi-techpark/opendatahub-go-sdk/qmill"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/trace"
)

// Data write notifications generated by the MongoDB Notifier
type Notification struct {
	Urn string
}

type RdbEnv = rdb.Env
type Env struct {
	RdbEnv
	MQ_URI      string
	MQ_EXCHANGE string `default:"routed"`
	MQ_CLIENT   string
	MQ_QUEUE    string
	MQ_KEY      string
	MONGO_URI   string
}

type Handler[P any] func(context.Context, *rdb.Raw[P]) error

type Tr[P any] struct {
	data_bridge *rdb.RDBridge
	config      Env
	sub         *qmill.QMill
}

func NewTr[P any](ctx context.Context, config Env) *Tr[P] {
	sub, err := qmill.NewSubscriberQmill(ctx, config.MQ_URI, config.MQ_CLIENT,
		qmill.WithQueue(config.MQ_QUEUE, true),
		qmill.WithBind(config.MQ_EXCHANGE, config.MQ_KEY),
		qmill.WithNoRequeueOnNack(true),
		qmill.WithLogger(watermill.NewSlogLogger(slog.Default())),
	)
	if err != nil {
		logger.Get(ctx).Error("failed to initialize Tr pub", "err", err)
		panic(err)
	}

	return &Tr[P]{
		data_bridge: rdb.NewRDBridge(config.RdbEnv),
		config:      config,
		sub:         sub,
	}
}

func (tr *Tr[P]) Start(ctx context.Context, handler Handler[P]) error {
	for msg := range tr.sub.Sub() {
		ctx := msg.Context()

		log := logger.Get(ctx)
		log.Debug("Received a message", "body", msg.Payload)

		if err := tr.handleDelivery(ctx, msg, handler); err != nil {
			tel.OnError(ctx, "Message handling failed", err)
		}
		tel.OnSuccess(ctx)
		trace.SpanFromContext(ctx).End()
	}
	err := errors.New("Tr unexpected sub channel close")
	logger.Get(ctx).Error(err.Error())
	return err
}

func (tr *Tr[P]) handleDelivery(ctx context.Context, delivery *message.Message, handler Handler[P]) error {
	msgBody := Notification{}
	if err := json.Unmarshal(delivery.Payload, &msgBody); err != nil {
		delivery.Nack()
		return fmt.Errorf("error unmarshalling mq message: %w", err)
	}

	u, ok := urn.Parse(msgBody.Urn)
	if !ok {
		return fmt.Errorf("invalid urn format in mq message: %s", msgBody.Urn)
	}

	rawFrame, err := rdb.Get[P](tr.data_bridge, ctx, u)
	if err != nil {
		delivery.Nack()
		return fmt.Errorf("cannot get raw data: %w", err)
	}

	err = handler(ctx, &rawFrame)
	if err != nil {
		delivery.Nack()
		return fmt.Errorf("error during handling of message: %w", err)
	}

	delivery.Ack()
	return nil
}
