package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/segmentio/kafka-go"

	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/tracing"
)

// Handler обрабатывает одно доменное событие. Реализуется Dispatcher; интерфейс
// объявлен здесь, чтобы Consumer не зависел от конкретного диспетчера.
type Handler interface {
	Handle(ctx context.Context, ev events.Event) error
}

// Consumer читает топик booking-events как участник consumer-group и отдаёт каждое
// событие Handler'у. Offset коммитится вручную и только после успешной обработки —
// это и есть гарантия at-least-once (падение до коммита → повторная доставка).
type Consumer struct {
	reader  *kafka.Reader
	handler Handler
	log     *slog.Logger
}

// NewConsumer создаёт Consumer поверх kafka.Reader с consumer-group. Новая группа
// читает топик с начала (FirstOffset), чтобы не терять уже опубликованные события;
// далее позиция хранится в закоммиченных offset'ах группы. log == nil → Default.
func NewConsumer(brokers []string, topic, groupID string, handler Handler, log *slog.Logger) *Consumer {
	if log == nil {
		log = slog.Default()
	}
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     brokers,
		Topic:       topic,
		GroupID:     groupID,
		StartOffset: kafka.FirstOffset,
		MinBytes:    1,
		MaxBytes:    10 << 20,
	})
	return &Consumer{reader: r, handler: handler, log: log}
}

// Run вычитывает сообщения до отмены ctx. Для каждого: восстанавливает
// trace-контекст из заголовков, декодирует Event, отдаёт Handler'у и — только при
// успехе — коммитит offset. Отмена ctx завершает цикл без ошибки.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}

		// Восстанавливаем trace-контекст продюсера из заголовка сообщения, чтобы
		// трейс не рвался на границе сервисов (симметрично publishBookingEvent).
		mctx := events.ContextFromMessage(ctx, msg)

		var ev events.Event
		if err := json.Unmarshal(msg.Value, &ev); err != nil {
			// Нераспарсиваемое сообщение не ретраится: иначе оно навсегда заблокирует
			// партицию. Логируем и коммитим, чтобы двигаться дальше.
			c.log.ErrorContext(mctx, "decode event, skipping poison message",
				"trace_id", tracing.TraceID(mctx), slog.Any("error", err))
			c.commit(ctx, msg)
			continue
		}

		if err := c.handler.Handle(mctx, ev); err != nil {
			// Транзиентный сбой обработки: offset НЕ коммитим — событие вернётся при
			// перезапуске/ребалансе (at-least-once).
			c.log.ErrorContext(mctx, "handle event, will redeliver",
				"trace_id", tracing.TraceID(mctx), "event_id", ev.EventID, slog.Any("error", err))
			continue
		}

		c.commit(ctx, msg)
	}
}

func (c *Consumer) commit(ctx context.Context, msg kafka.Message) {
	if err := c.reader.CommitMessages(ctx, msg); err != nil {
		c.log.Error("commit offset", slog.Any("error", err))
	}
}

// Close закрывает Reader, фиксируя выход из группы.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
