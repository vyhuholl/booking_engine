package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/tracing"
)

// Зависимости диспетчера объявлены здесь, в потребителе, — это и есть граница
// слоя (аналогично тому, как service объявляет свои Repo-интерфейсы).

// AdminLookup отдаёт всех админов — адресатов события booking.pending_approval.
type AdminLookup interface {
	ListAdmins(ctx context.Context) ([]model.User, error)
}

// DedupStore — устойчивое хранилище идентификаторов обработанных событий
// (идемпотентность обработки). Ключ — event_id.
type DedupStore interface {
	IsProcessed(ctx context.Context, eventID string) (bool, error)
	MarkProcessed(ctx context.Context, eventID, eventType string) error
}

// DeadLetterStore принимает уведомления, не доставленные после всех ретраев.
type DeadLetterStore interface {
	SaveDeadLetter(ctx context.Context, eventID, userID, notificationType, reason string) error
}

// BookingLookup обогащает уведомление данными брони (причина отказа для
// booking.rejected). Опциональна: без неё уведомление несёт только идентификаторы.
type BookingLookup interface {
	Get(ctx context.Context, id string) (model.Booking, error)
}

// Config — параметры повторной доставки.
type Config struct {
	RetryMax  int           // число попыток Notifier.Send (>= 1)
	RetryBase time.Duration // базовая задержка backoff: base * 2^(attempt-1)
}

// Option настраивает необязательные зависимости диспетчера.
type Option func(*Dispatcher)

// WithBookingLookup включает обогащение уведомлений данными брони (причина отказа
// для booking.rejected).
func WithBookingLookup(b BookingLookup) Option {
	return func(d *Dispatcher) { d.bookings = b }
}

// Dispatcher превращает доменное событие в уведомления адресатам: проверяет дедуп,
// маппит тип события на получателей и содержимое, доставляет через Notifier с
// retry/backoff, при исчерпании попыток пишет в dead-letter. Бизнес-логика
// обработки события — здесь; Consumer лишь читает Kafka и делегирует сюда.
type Dispatcher struct {
	notifier   Notifier
	admins     AdminLookup
	dedup      DedupStore
	deadLetter DeadLetterStore
	bookings   BookingLookup // опционально (см. WithBookingLookup)
	cfg        Config
	log        *slog.Logger
}

// NewDispatcher собирает диспетчер. log == nil заменяется на slog.Default();
// RetryMax < 1 поднимается до 1 (хотя бы одна попытка).
func NewDispatcher(n Notifier, admins AdminLookup, dedup DedupStore, dl DeadLetterStore, cfg Config, log *slog.Logger, opts ...Option) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	if cfg.RetryMax < 1 {
		cfg.RetryMax = 1
	}
	d := &Dispatcher{notifier: n, admins: admins, dedup: dedup, deadLetter: dl, cfg: cfg, log: log}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Handle обрабатывает одно событие идемпотентно. Порядок строгий: сначала
// дедуп-проверка, затем доставка адресатам, и только после — фиксация event_id
// как обработанного. Возврат ошибки означает «не коммить offset» (транзиентный
// сбой — событие вернётся при повторной доставке, at-least-once); nil — «offset
// можно коммитить» (обработано, включая уход в dead-letter).
func (d *Dispatcher) Handle(ctx context.Context, ev events.Event) error {
	key := dedupKey(ev)
	log := d.log.With(
		"trace_id", tracing.TraceID(ctx),
		"event_id", key,
		"type", ev.Type,
		"booking_id", ev.BookingID,
	)
	if ev.EventID == "" {
		log.Warn("event without event_id, using synthetic dedup key")
	}

	processed, err := d.dedup.IsProcessed(ctx, key)
	if err != nil {
		return fmt.Errorf("dedup check: %w", err)
	}
	if processed {
		log.Info("event already processed, skipping")
		return nil
	}

	recipients, n, handled, err := d.plan(ctx, ev)
	if err != nil {
		return err
	}
	if !handled {
		log.Info("unhandled event type, skipping")
		return nil
	}

	for _, userID := range recipients {
		if err := d.deliver(ctx, userID, n); err != nil {
			log.Error("notification dead-lettered", "user_id", userID, slog.Any("error", err))
			if dlErr := d.deadLetter.SaveDeadLetter(ctx, key, userID, n.Type, err.Error()); dlErr != nil {
				log.Error("save dead letter", "user_id", userID, slog.Any("error", dlErr))
			}
			continue
		}
		log.Info("notification sent", "user_id", userID, "notification_type", n.Type)
	}

	if err := d.dedup.MarkProcessed(ctx, key, ev.Type); err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	return nil
}

// plan определяет адресатов и содержимое уведомления по типу события.
// handled == false для неизвестного типа (событие пропускается без ошибки).
func (d *Dispatcher) plan(ctx context.Context, ev events.Event) (recipients []string, n Notification, handled bool, err error) {
	switch ev.Type {
	case events.TypeBookingCreated:
		return []string{ev.UserID}, notif(NotifyBookingConfirmed, ev, "Бронирование подтверждено"), true, nil
	case events.TypeBookingCancelled:
		return []string{ev.UserID}, notif(NotifyBookingCancelled, ev, "Бронирование отменено"), true, nil
	case events.TypeBookingApproved:
		return []string{ev.UserID}, notif(NotifyBookingApproved, ev, "Бронь одобрена"), true, nil
	case events.TypeBookingRejected:
		n := notif(NotifyBookingRejected, ev, "Бронь отклонена")
		n.Reason = d.rejectionReason(ctx, ev.BookingID)
		return []string{ev.UserID}, n, true, nil
	case events.TypeBookingPendingApproval:
		admins, err := d.admins.ListAdmins(ctx)
		if err != nil {
			return nil, Notification{}, false, fmt.Errorf("list admins: %w", err)
		}
		ids := make([]string, 0, len(admins))
		for _, a := range admins {
			ids = append(ids, a.ID)
		}
		return ids, notif(NotifyApprovalRequested, ev, "Требуется одобрение брони"), true, nil
	default:
		return nil, Notification{}, false, nil
	}
}

// rejectionReason достаёт причину отказа из брони, если включён BookingLookup.
// Ошибка/недоступность лукапа не роняет уведомление — оно уйдёт без причины
// (деградация, а не отказ доставки).
func (d *Dispatcher) rejectionReason(ctx context.Context, bookingID string) string {
	if d.bookings == nil {
		return ""
	}
	b, err := d.bookings.Get(ctx, bookingID)
	if err != nil {
		d.log.Warn("rejection reason lookup failed, notifying without reason",
			"booking_id", bookingID, slog.Any("error", err))
		return ""
	}
	if b.RejectionReason == nil {
		return ""
	}
	return *b.RejectionReason
}

// deliver отправляет уведомление с экспоненциальным backoff до RetryMax попыток.
// Возвращает ошибку последней попытки, если все исчерпаны, либо ctx.Err() при
// отмене во время ожидания.
func (d *Dispatcher) deliver(ctx context.Context, userID string, n Notification) error {
	var err error
	for attempt := 1; attempt <= d.cfg.RetryMax; attempt++ {
		if attempt > 1 {
			backoff := d.cfg.RetryBase * (1 << (attempt - 2))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = d.notifier.Send(ctx, userID, n); err == nil {
			return nil
		}
		d.log.Warn("notifier send failed, will retry",
			"user_id", userID, "attempt", attempt, "max", d.cfg.RetryMax, slog.Any("error", err))
	}
	return err
}

func notif(typ string, ev events.Event, title string) Notification {
	return Notification{Type: typ, BookingID: ev.BookingID, RoomID: ev.RoomID, Title: title}
}

// dedupKey — ключ дедупликации. По умолчанию event_id; для «старых» событий без
// event_id синтезируется из type|booking_id|timestamp, чтобы дедуп не ломался на
// пустом ключе.
func dedupKey(ev events.Event) string {
	if ev.EventID != "" {
		return ev.EventID
	}
	return fmt.Sprintf("%s|%s|%d", ev.Type, ev.BookingID, ev.Timestamp.UnixNano())
}
