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
)

// Event — доменное событие бронирования, публикуемое в шину.
// Идентификаторы хранятся строками в формате всего проекта: b-<uuid>,
// user-<uuid>, room-<uuid>.
type Event struct {
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
