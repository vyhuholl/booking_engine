// Package events описывает доменные события бронирования и абстракцию их
// публикации в шину. Пакет намеренно не зависит от service/model: событие
// переносит только идентификаторы (в формате префикс+uuid, как во всём проекте)
// и не тянет за собой доменные типы.
package events

import (
	"context"
	"time"
)

// Типы событий бронирования. Значение — то, что уходит в поле Event.Type.
const (
	TypeBookingCreated   = "booking.created"
	TypeBookingCancelled = "booking.cancelled"

	// Workflow одобрения больших переговорок (change add-large-room-approval):
	// смена статуса брони публикуется отдельными событиями.
	TypeBookingPendingApproval = "booking.pending_approval" // создана бронь на согласование
	TypeBookingApproved        = "booking.approved"         // admin одобрил
	TypeBookingRejected        = "booking.rejected"         // admin отклонил / авто-reject по таймауту
)

// Event — доменное событие бронирования, публикуемое в шину.
// Идентификаторы хранятся строками в формате всего проекта: b-<uuid>,
// user-<uuid>, room-<uuid>.
type Event struct {
	// EventID — стабильный идентификатор события (формат evt-<uuid>). Ключ
	// дедупликации у потребителей (см. internal/notifications): одно и то же
	// событие при повторной доставке несёт тот же EventID.
	EventID   string    `json:"event_id"`
	Type      string    `json:"type"`
	BookingID string    `json:"booking_id"`
	UserID    string    `json:"user_id"`
	RoomID    string    `json:"room_id"`
	Timestamp time.Time `json:"timestamp"`
}

// EventPublisher публикует событие в указанный топик. Реализация обязана быть
// потокобезопасной: один паблишер разделяется всеми запросами.
type EventPublisher interface {
	Publish(ctx context.Context, topic string, event Event) error
}
