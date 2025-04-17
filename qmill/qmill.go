// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package qmill

import (
	"context"
	"fmt"

	"errors"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-amqp/v2/pkg/amqp"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/ms"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// QMill wraps an AMQP config from Watermill and forces declaration
// of exchanges and queues if requested.
type QMill struct {
	config                 *amqp.Config
	logger                 watermill.LoggerAdapter
	forceExchange          bool // force exchange declaration (e.g. for producers)
	forceQueue             bool // force queue declaration (e.g. for consumers)
	altExchange            string
	deadLetterExchange     string
	altExchangeType        string
	deadLetterExchangeType string
	routingKey             string
	routingExchangeName    string

	pub *amqp.Publisher
	sub <-chan *message.Message
}

// Option defines a function type that modifies the QMill.
type Option func(*QMill)

// WithExchange sets a fixed exchange name and the exchange type (e.g. "direct", "topic", "fanout").
func WithExchange(name, exchangeType string, declare bool) Option {
	return func(a *QMill) {
		a.config.Exchange.GenerateName = func(_ string) string { return name }
		if exchangeType == "topic" {
			a.config.Publish.GenerateRoutingKey = func(topic string) string { return topic }
		}
		a.config.Exchange.Type = exchangeType
		a.forceExchange = declare
	}
}

// WithQueue sets a constant queue name.
func WithQueue(name string, declare bool) Option {
	return func(a *QMill) {
		a.config.Queue.GenerateName = amqp.GenerateQueueNameConstant(name)
		a.forceQueue = declare
	}
}

// WithBind sets a bind between a queue and an eschange.
func WithBind(exchange, routingKey string) Option {
	return func(a *QMill) {
		a.routingExchangeName = exchange
		a.routingKey = routingKey
	}
}

// WithDeadLetter sets the dead-letter exchange for the queue declaration.
// Deadletter exchange is where a message is sent after the Nack count exceeds MaxDeliveryCount.
// It adds an "x-dead-letter-exchange" argument to the queue and stores the name so that the dead-letter exchange can be declared.
// EschangeType can be "direct", "topic", "fanout".
func WithDeadLetter(deadLetterExchange string, exchangeType string) Option {
	return func(a *QMill) {
		if a.config.Queue.Arguments == nil {
			a.config.Queue.Arguments = amqp091.Table{}
		}
		a.config.Queue.Arguments["x-dead-letter-exchange"] = deadLetterExchange
		a.deadLetterExchange = deadLetterExchange
		a.deadLetterExchangeType = exchangeType
	}
}

// WithAlternateExchange sets an alternate exchange on the main exchange declaration.
// Alternate exchange is where a message is sent if it is "unroutable" (normally by routing key) from the primary exchange.
// It adds an "alternate-exchange" argument and stores the alternate exchange name so that it can be declared.
// EschangeType can be "direct", "topic", "fanout".
func WithAlternateExchange(alternateExchange string, exchangeType string) Option {
	return func(a *QMill) {
		if a.config.Exchange.Arguments == nil {
			a.config.Exchange.Arguments = amqp091.Table{}
		}
		a.config.Exchange.Arguments["alternate-exchange"] = alternateExchange
		a.altExchange = alternateExchange
		a.altExchangeType = exchangeType
	}
}

// WithNoRequeueOnNack sets the NoRequeueOnNack flag for consumers.
// When set to true, the broker will not requeue messages on negative acknowledgments.
func WithNoRequeueOnNack(noRequeue bool) Option {
	return func(a *QMill) {
		a.config.Consume.NoRequeueOnNack = noRequeue
	}
}

// WithMaxDeliveryCount sets the maximum number of deliveries a message can have
// before it is dead-lettered. This is added as a queue argument and tells RabbitMQ
// how many times to redeliver a message before sending it to the dead-letter exchange.
// Note: The actual behavior depends on your RabbitMQ version/queue type or plugin support.
func WithMaxDeliveryCount(max int) Option {
	return func(a *QMill) {
		if a.config.Queue.Arguments == nil {
			a.config.Queue.Arguments = amqp091.Table{}
		}
		a.config.Queue.Arguments["x-max-delivery-count"] = max
	}
}

// WithLogger sets the logger
func WithLogger(logger watermill.LoggerAdapter) Option {
	return func(a *QMill) {
		a.logger = logger
	}
}

// NewQmill creates a new instance of QMill using the given URI and options.
// It starts with Watermill’s NewDurablePubSubConfig as a base configuration.
func NewQmill(uri, client_name string, opts ...Option) (*QMill, error) {
	// Create a default config using topic name generator by default.
	config := amqp.NewDurablePubSubConfig(uri, amqp.GenerateQueueNameTopicName)
	// set client name
	config.Connection.AmqpConfig = &amqp091.Config{
		Properties: amqp091.Table{"connection_name": client_name},
	}

	logger := watermill.NewStdLogger(false, false)

	decl := &QMill{
		config:                 &config,
		logger:                 logger,
		forceQueue:             false,
		forceExchange:          false,
		altExchange:            "",
		deadLetterExchange:     "",
		altExchangeType:        "",
		deadLetterExchangeType: "",
		routingKey:             "",
		routingExchangeName:    "",
		pub:                    nil,
		sub:                    nil,
	}

	// Apply all provided options.
	for _, opt := range opts {
		opt(decl)
	}

	config.Marshaler = newMarshalWrapper(decl.config.Queue.GenerateName(""))

	// If any forced declaration or extra exchanges are requested, perform them now.
	if decl.forceExchange || decl.forceQueue || decl.altExchange != "" || decl.deadLetterExchange != "" {
		if err := decl.forceDeclarations(); err != nil {
			return nil, err
		}
	}

	return decl, nil
}

func NewSubscriberQmill(ctx context.Context, uri, client_name string, opts ...Option) (*QMill, error) {
	q, err := NewQmill(uri, client_name, opts...)
	if err != nil {
		return nil, err
	}
	if err := q.Subscribe(ctx); err != nil {
		return nil, err
	}
	return q, nil
}

func NewPublisherQmill(ctx context.Context, uri, client_name string, opts ...Option) (*QMill, error) {
	q, err := NewQmill(uri, client_name, opts...)
	if err != nil {
		return nil, err
	}
	if err := q.PreparePublisher(ctx); err != nil {
		return nil, err
	}
	return q, nil
}

// forceDeclarations opens a raw AMQP connection and channel to declare resources.
// It declares the exchange if forceExchange is set, and the queue (with binding) if forceQueue is set.
// Note that you may choose to force only one side depending on your producer/consumer requirements.
// forceDeclarations opens a raw AMQP connection and channel to declare resources.
// It declares:
//   - The alternate exchange if specified (using the specified type or default "fanout").
//   - The dead-letter exchange if specified (using the specified type or default "direct").
//   - The main exchange if forceExchange is enabled.
//   - The queue (and its binding) if forceQueue is enabled.
func (a *QMill) forceDeclarations() error {
	conn, err := amqp091.Dial(a.config.Connection.AmqpURI)
	if err != nil {
		return fmt.Errorf("failed to dial AMQP: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open channel: %w", err)
	}
	defer ch.Close()

	// Declare the alternate exchange if specified.
	if a.altExchange != "" {
		exchType := a.altExchangeType
		if exchType == "" {
			exchType = "fanout"
		}
		if err = ch.ExchangeDeclare(
			a.altExchange,
			exchType,
			true,  // durable
			false, // autoDeleted
			false, // internal
			false, // noWait: wait for confirmation
			nil,
		); err != nil {
			return fmt.Errorf("failed to declare alternate exchange: %w", err)
		}
	}

	// Declare the dead-letter exchange if specified.
	if a.deadLetterExchange != "" {
		exchType := a.deadLetterExchangeType
		if exchType == "" {
			exchType = "direct"
		}
		if err = ch.ExchangeDeclare(
			a.deadLetterExchange,
			exchType,
			true,  // durable
			false, // autoDeleted
			false, // internal
			false, // noWait: wait for confirmation
			nil,
		); err != nil {
			return fmt.Errorf("failed to declare dead-letter exchange: %w", err)
		}
	}

	// Declare the main exchange if forced.
	if a.forceExchange {
		exchangeName := a.config.Exchange.GenerateName("")
		if err = ch.ExchangeDeclare(
			exchangeName,
			a.config.Exchange.Type,
			a.config.Exchange.Durable,
			a.config.Exchange.AutoDeleted,
			false, // internal
			false, // noWait: wait for confirmation
			a.config.Exchange.Arguments,
		); err != nil {
			return fmt.Errorf("failed to declare main exchange: %w", err)
		}
	}

	// Declare the queue (and bind it) if forced.
	if a.forceQueue && a.config.Queue.GenerateName != nil {
		queueName := a.config.Queue.GenerateName("")
		if _, err = ch.QueueDeclare(
			queueName,
			a.config.Queue.Durable,
			false,
			a.config.Queue.Exclusive,
			false, // false: wait for confirmation
			a.config.Queue.Arguments,
		); err != nil {
			return fmt.Errorf("failed to declare queue: %w", err)
		}

		// Bind the queue to the main exchange if available.
		if a.routingExchangeName != "" {
			if err = ch.QueueBind(
				queueName,
				a.routingKey,
				a.routingExchangeName,
				false, // noWait
				nil,
			); err != nil {
				return fmt.Errorf("failed to bind queue: %w", err)
			}
		}
	}

	return nil
}

// PreparePublisher returns a Watermill AMQP Publisher based on the current configuration.
func (a *QMill) PreparePublisher(ctx context.Context) error {
	pub, err := amqp.NewPublisher(*a.config, a.logger)
	if err != nil {
		return err
	}

	a.pub = pub
	return nil
}

func (q *QMill) Publish(ctx context.Context, payload []byte, dest string) error {
	if q.pub == nil {
		return errors.New("publishing on an uninitialized qmill")
	}
	msg := message.NewMessage(watermill.NewUUID(), payload)

	if !tel.Enabled() {
		return q.pub.Publish(dest, msg)
	}

	// 3. Create a producer span
	pubCtx, producerSpan := tel.TraceStart(
		ctx,
		fmt.Sprintf("%s.publish", tel.GetServiceName()),
		trace.WithSpanKind(trace.SpanKindProducer),
	)
	defer producerSpan.End()

	// Set some producer-related attributes
	producerSpan.SetAttributes(
		attribute.String("messaging.system", "rabbitmq"),
		attribute.String("messaging.destination_kind", "exchange"),
		attribute.String("messaging.destination", dest),
	)

	// 4. Inject trace context into message metadata using a TextMapCarrier
	// Assumption: msg.Metadata is essentially a map[string]string
	carrier := propagation.MapCarrier(msg.Metadata)
	otel.GetTextMapPropagator().Inject(pubCtx, carrier)

	return q.pub.Publish(dest, msg)
}

// Subscribe setup the Watermill AMQP Subscriber based on the current configuration.
// The channel is then usable using q.Sub()
func (q *QMill) Subscribe(ctx context.Context) error {
	sub, err := amqp.NewSubscriber(*q.config, q.logger)
	if err != nil {
		return err
	}

	inCh, err := sub.Subscribe(ctx, "")
	if err != nil {
		return err
	}

	if !tel.Enabled() {
		return nil
	}

	outCh := make(chan *message.Message)
	go func() {
		defer close(outCh)
		for msg := range inCh {
			// 1. Use propagation.MapCarrier to adapt msg.Metadata as a TextMapCarrier.
			carrier := propagation.MapCarrier(msg.Metadata)
			// Extract the span context from the message using the global propagator.
			mCtx := otel.GetTextMapPropagator().Extract(msg.Context(), carrier)

			// 2. Start consumer span
			mCtx, _ = tel.TraceStart(
				mCtx,
				fmt.Sprintf("%s.consumer", tel.GetServiceName()),
				trace.WithSpanKind(trace.SpanKindConsumer),
			)

			// 3. Spawn a new slog logger that includes IDs for correlation
			mCtx = logger.WithTracedLogger(mCtx)

			// Set the new context on the message
			msg.SetContext(mCtx)
			outCh <- msg
		}
	}()

	q.sub = outCh
	return nil
}

func (q *QMill) Sub() <-chan *message.Message {
	if q.sub == nil {
		panic("sub on an uninitialized qmill")
	}
	return q.sub
}

// failOnError records error info on the span, flushes pending spans, and panics.
func (q *QMill) FailOnError(err error, msg string, message *message.Message, args ...any) {
	if err == nil {
		return
	}

	message.Nack()
	ms.FailOnError(message.Context(), err, msg, args...)
}

// marshalWrapper wraps the DefaultMarshaler but customizes Unmarshal.
type marshalWrapper struct {
	defaultMarshaller amqp.DefaultMarshaler
	queue             string
}

// newMarshalWrapper returns a new instance of the wrapper.
func newMarshalWrapper(queue string) marshalWrapper {
	return marshalWrapper{
		defaultMarshaller: amqp.DefaultMarshaler{},
		queue:             queue,
	}
}

// Marshal simply delegates to the default marshaller.
func (m marshalWrapper) Marshal(msg *message.Message) (amqp091.Publishing, error) {
	return m.defaultMarshaller.Marshal(msg)
}

// Unmarshal filters non-string headers and retains Exchange/RoutingKey.
func (m marshalWrapper) Unmarshal(amqpMsg amqp091.Delivery) (*message.Message, error) {
	// Use DefaultMarshaler to get the base message
	msg, err := m.defaultMarshaller.Unmarshal(amqpMsg)
	if err != nil {
		return nil, err
	}

	// Filter out non-string headers
	cleanedMetadata := message.Metadata{}
	for key, value := range amqpMsg.Headers {
		if strVal, ok := value.(string); ok {
			cleanedMetadata[key] = strVal
		}
	}

	// Preserve RabbitMQ metadata
	cleanedMetadata["rabbitmq.exchange"] = amqpMsg.Exchange
	cleanedMetadata["rabbitmq.routing_key"] = amqpMsg.RoutingKey
	cleanedMetadata["rabbitmq.queue"] = m.queue

	// Set filtered metadata
	msg.Metadata = cleanedMetadata

	return msg, nil
}
