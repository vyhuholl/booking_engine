package events

import (
	"context"
	"encoding/json"

	"github.com/segmentio/kafka-go"
)

// KafkaPublisher реализует EventPublisher поверх kafka-go Writer.
// Топик не фиксируется в Writer'е, а задаётся на каждое сообщение — так один
// паблишер обслуживает произвольные топики через аргумент Publish.
type KafkaPublisher struct {
	writer *kafka.Writer
}

// NewKafkaPublisher создаёт паблишер, пишущий в брокеры brokers.
func NewKafkaPublisher(brokers []string) *KafkaPublisher {
	return &KafkaPublisher{
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true,
		},
	}
}

// Publish сериализует событие в JSON и пишет его в topic. Ключом сообщения
// служит BookingID: события одной брони попадают в один партишн и сохраняют
// порядок (created раньше cancelled).
func (p *KafkaPublisher) Publish(ctx context.Context, topic string, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return p.writer.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   []byte(event.BookingID),
		Value: payload,
	})
}

// Close закрывает Writer, дожидаясь отправки буферизованных сообщений.
func (p *KafkaPublisher) Close() error {
	return p.writer.Close()
}
