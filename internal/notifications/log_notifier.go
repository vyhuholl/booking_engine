package notifications

import (
	"context"
	"log/slog"

	"github.com/example/booking-engine/internal/tracing"
)

// LogNotifier — реализация-заглушка Notifier: пишет уведомление в структурный лог
// и всегда возвращает nil. Логирует только идентификаторы (user_id, type,
// booking_id) — без PII и секретов. Реальные каналы доставки (email/WebSocket)
// подключаются позже за тем же интерфейсом.
type LogNotifier struct {
	log *slog.Logger
}

// NewLogNotifier создаёт LogNotifier. log == nil заменяется на slog.Default().
func NewLogNotifier(log *slog.Logger) *LogNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &LogNotifier{log: log}
}

func (l *LogNotifier) Send(ctx context.Context, userID string, n Notification) error {
	l.log.InfoContext(ctx, "notification delivered",
		"trace_id", tracing.TraceID(ctx),
		"user_id", userID,
		"notification_type", n.Type,
		"booking_id", n.BookingID,
	)
	return nil
}
