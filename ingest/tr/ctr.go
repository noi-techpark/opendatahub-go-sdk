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
	"github.com/noi-techpark/opendatahub-go-sdk/qmill"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/trace"
)

type CHandler[P any] func(context.Context, *P) error

type CTr[P any] struct {
	config Env
	sub    *qmill.QMill
}

func NewCTr[P any](ctx context.Context, config Env) *CTr[P] {
	sub, err := qmill.NewSubscriberQmill(ctx, config.MQ_URI, config.MQ_CLIENT,
		qmill.WithQueue(config.MQ_QUEUE, true),
		qmill.WithBind(config.MQ_EXCHANGE, config.MQ_KEY),
		qmill.WithNoRequeueOnNack(true),
		qmill.WithLogger(watermill.NewSlogLogger(slog.Default())),
	)
	if err != nil {
		logger.Get(ctx).Error("failed to initialize CTr pub", "err", err)
		panic(err)
	}

	return &CTr[P]{
		config: config,
		sub:    sub,
	}
}

func (tr *CTr[P]) Start(ctx context.Context, handler CHandler[P]) error {
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
	err := errors.New("CTr unexpected sub channel close")
	logger.Get(ctx).Error(err.Error())
	return err
}

func (tr *CTr[P]) handleDelivery(ctx context.Context, delivery *message.Message, handler CHandler[P]) error {
	var msgBody P
	if err := json.Unmarshal(delivery.Payload, &msgBody); err != nil {
		delivery.Nack()
		return fmt.Errorf("error unmarshalling mq message: %w", err)
	}

	err := handler(ctx, &msgBody)
	if err != nil {
		delivery.Nack()
		return fmt.Errorf("error during handling of message: %w", err)
	}

	delivery.Ack()
	return nil
}
