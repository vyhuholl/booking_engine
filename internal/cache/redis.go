package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/tracing"
)

const (
	// keyPrefix — общий префикс ключей доступности; keyPattern матчит их все
	// для инвалидации через SCAN.
	keyPrefix  = "rooms:available:"
	keyPattern = keyPrefix + "*"
	// scanCount — хинт размера пачки для SCAN (не гарантия, а подсказка Redis).
	scanCount = 100
)

// RedisCache — реализация RoomCacheInterface поверх go-redis. Клиент
// потокобезопасен, поэтому один экземпляр разделяется всеми запросами.
type RedisCache struct {
	client *redis.Client
	ttl    time.Duration
	log    *slog.Logger
}

// RedisOption настраивает RedisCache при создании.
type RedisOption func(*RedisCache)

// WithTTL переопределяет время жизни записей (по умолчанию TTL). Нужен прежде
// всего в тестах, где ждать полный пятиминутный TTL непрактично.
func WithTTL(d time.Duration) RedisOption {
	return func(c *RedisCache) { c.ttl = d }
}

// NewRedis собирает кэш поверх готового клиента. Клиентом владеет вызывающий
// (он же закрывает его при завершении). log == nil заменяется на slog.Default().
func NewRedis(client *redis.Client, log *slog.Logger, opts ...RedisOption) *RedisCache {
	if log == nil {
		log = slog.Default()
	}
	c := &RedisCache{client: client, ttl: TTL, log: log}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// availableKey формирует ключ окна: rooms:available:{start_unix}:{end_unix}.
// Unix-время не зависит от таймзоны, поэтому одинаковые абсолютные моменты дают
// один ключ независимо от зоны аргументов.
func availableKey(start, end time.Time) string {
	return fmt.Sprintf("%s%d:%d", keyPrefix, start.Unix(), end.Unix())
}

func (c *RedisCache) GetAvailableRooms(ctx context.Context, start, end time.Time) ([]model.Room, error) {
	data, err := c.client.Get(ctx, availableKey(start, end)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrCacheMiss
		}
		return nil, err
	}
	var rooms []model.Room
	if err := json.Unmarshal(data, &rooms); err != nil {
		return nil, err
	}
	c.log.Debug("room cache hit", "trace_id", tracing.TraceID(ctx), "count", len(rooms))
	return rooms, nil
}

func (c *RedisCache) SetAvailableRooms(ctx context.Context, start, end time.Time, rooms []model.Room) error {
	data, err := json.Marshal(rooms)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, availableKey(start, end), data, c.ttl).Err()
}

// InvalidateRoomCache сбрасывает весь срез доступности. Ключ окна агрегирует все
// комнаты и не содержит room_id, поэтому точечно удалить записи одной комнаты
// нельзя — удаляем все rooms:available:*. roomID оставлен в сигнатуре под будущий
// индекс «комната → окна» и для логирования на стороне вызова.
func (c *RedisCache) InvalidateRoomCache(ctx context.Context, roomID string) error {
	iter := c.client.Scan(ctx, 0, keyPattern, scanCount).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	if err := c.client.Del(ctx, keys...).Err(); err != nil {
		return err
	}
	c.log.Debug("room cache invalidated", "trace_id", tracing.TraceID(ctx), "room_id", roomID, "keys", len(keys))
	return nil
}
