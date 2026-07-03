// Package notifications потребляет доменные события бронирования из Kafka
// (отдельным процессом cmd/notifier) и доставляет уведомления адресатам через
// абстракцию Notifier. Обработка идемпотентна (дедуп по event_id), гарантия
// доставки — at-least-once, при ошибке — retry с backoff и dead-letter.
package notifications

import "context"

// Типы уведомлений — доменный смысл события для адресата. Значение попадает в
// Notification.Type и в лог/dead-letter.
const (
	NotifyBookingConfirmed  = "booking_confirmed"  // booking.created → владелец
	NotifyBookingCancelled  = "booking_cancelled"  // booking.cancelled → владелец
	NotifyApprovalRequested = "approval_requested" // booking.pending_approval → все админы
	NotifyBookingApproved   = "booking_approved"   // booking.approved → создатель
	NotifyBookingRejected   = "booking_rejected"   // booking.rejected → создатель (с причиной)
)

// Notification — уведомление пользователю. Несёт идентификаторы брони (в формате
// проекта: b-<uuid>/room-<uuid>) и человекочитаемые заголовок/текст; Reason
// заполняется только для NotifyBookingRejected (причина отказа).
type Notification struct {
	Type      string
	BookingID string
	RoomID    string
	Title     string
	Message   string
	Reason    string
}

// Notifier доставляет уведомление адресату. userID — идентификатор пользователя в
// формате проекта (user-<uuid>). Реализация обязана быть потокобезопасной: один
// Notifier разделяется обработкой всех событий.
type Notifier interface {
	Send(ctx context.Context, userID string, n Notification) error
}
