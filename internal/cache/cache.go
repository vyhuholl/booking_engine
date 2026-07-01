// Package cache описывает кэш списка свободных комнат и его абстракцию для
// сервисного слоя. Интерфейс RoomCacheInterface объявлен здесь (а не в
// потребителе) по той же причине, что и events.EventPublisher: это
// инфраструктурная абстракция с собственной реализацией, разделяемой всеми
// запросами. Реализация обязана быть потокобезопасной.
package cache

import (
	"context"
	"errors"
	"time"

	"github.com/example/booking-engine/internal/model"
)

// TTL — время жизни записи о свободных комнатах. Короткий срок ограничивает
// рассогласование, если инвалидация по какой-то причине не сработала.
const TTL = 5 * time.Minute

// ErrCacheMiss возвращается GetAvailableRooms, когда для окна нет записи.
// Отличать промах от «в кэше пустой список» важно: пустой срез — валидное
// закэшированное значение (свободных комнат нет), а промах требует похода в БД.
var ErrCacheMiss = errors.New("cache_miss")

// RoomCacheInterface — кэш списка свободных комнат по окну [start, end).
type RoomCacheInterface interface {
	// GetAvailableRooms возвращает закэшированный список для окна или
	// ErrCacheMiss, если записи нет.
	GetAvailableRooms(ctx context.Context, start, end time.Time) ([]model.Room, error)
	// SetAvailableRooms кладёт список свободных комнат для окна с TTL.
	SetAvailableRooms(ctx context.Context, start, end time.Time, rooms []model.Room) error
	// InvalidateRoomCache сбрасывает кэш доступности, затронутый изменением
	// брони комнаты roomID.
	InvalidateRoomCache(ctx context.Context, roomID string) error
}
