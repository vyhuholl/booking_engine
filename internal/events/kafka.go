package events

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/segmentio/kafka-go"

	"github.com/example/booking-engine/internal/tracing"
)

// KafkaPublisher реализует EventPublisher поверх kafka-go Writer.
// Топик не фиксируется в Writer'е, а задаётся на каждое сообщение — так один
// паблишер обслуживает произвольные топики через аргумент Publish.
type KafkaPublisher struct {
	writer *kafka.Writer
	log    *slog.Logger
}

// NewKafkaPublisher создаёт паблишер, пишущий в брокеры brokers. log == nil
// заменяется на slog.Default().
func NewKafkaPublisher(brokers []string, log *slog.Logger) *KafkaPublisher {
	if log == nil {
		log = slog.Default()
	}
	return &KafkaPublisher{
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true,
		},
		log: log,
	}
}

// Publish сериализует событие в JSON и пишет его в topic. Ключом сообщения
// служит BookingID: события одной брони попадают в один партишн и сохраняют
// порядок (created раньше cancelled). Trace-контекст из ctx уходит в заголовок
// сообщения (см. newMessage), а не в тело Event.
func (p *KafkaPublisher) Publish(ctx context.Context, topic string, event Event) error {
	msg, err := newMessage(ctx, topic, event)
	if err != nil {
		return err
	}
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return err
	}
	p.log.Info("event published",
		"trace_id", tracing.TraceID(ctx),
		"type", event.Type,
		"booking_id", event.BookingID,
		"topic", topic,
	)
	return nil
}

// Close закрывает Writer, дожидаясь отправки буферизованных сообщений.
func (p *KafkaPublisher) Close() error {
	return p.writer.Close()
}

// newMessage собирает Kafka-сообщение из события и trace-контекста. W3C
// traceparent кладётся в заголовок сообщения (kafka.Header), а не в тело Event:
// так контракт Event не расширяется, а consumer восстанавливает контекст из
// заголовков (см. ContextFromMessage) — трейс не рвётся на границе сервисов.
func newMessage(ctx context.Context, topic string, event Event) (kafka.Message, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return kafka.Message{}, err
	}
	msg := kafka.Message{
		Topic: topic,
		Key:   []byte(event.BookingID),
		Value: payload,
	}
	if tp := tracing.Traceparent(ctx); tp != "" {
		msg.Headers = append(msg.Headers, kafka.Header{
			Key:   tracing.HeaderName,
			Value: []byte(tp),
		})
	}
	return msg, nil
}

// ContextFromMessage восстанавливает trace-контекст из заголовков Kafka-сообщения
// в ctx. Симметрична newMessage: consumer вызывает её на входе, чтобы продолжить
// трейс, начатый сервисом-продюсером. Если заголовка traceparent нет, ctx
// возвращается без изменений.
func ContextFromMessage(ctx context.Context, msg kafka.Message) context.Context {
	for _, h := range msg.Headers {
		if h.Key == tracing.HeaderName {
			return tracing.WithTraceparent(ctx, string(h.Value))
		}
	}
	return ctx
}
