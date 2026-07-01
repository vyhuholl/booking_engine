package cache_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/cache"
	"github.com/example/booking-engine/internal/model"
)

// newTestCache поднимает in-memory Redis (miniredis) и возвращает кэш поверх
// него плюс сам сервер (для проверок TTL/FastForward и низкоуровневых ключей).
func newTestCache(t *testing.T) (*cache.RedisCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return cache.NewRedis(client, nil), mr
}

var (
	winStart = time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	winEnd   = winStart.Add(time.Hour)
)

func sampleRooms() []model.Room {
	return []model.Room{
		{ID: "r-1", Name: "Alpha", Capacity: 4, Floor: 2, Equipment: []model.Equipment{model.EquipmentProjector}, Status: model.RoomStatusActive},
		{ID: "r-2", Name: "Beta", Capacity: 8, Floor: 3, Equipment: []model.Equipment{}, Status: model.RoomStatusActive},
	}
}

func TestRedisCache_GetMissReturnsSentinel(t *testing.T) {
	c, _ := newTestCache(t)

	got, err := c.GetAvailableRooms(context.Background(), winStart, winEnd)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
	assert.Nil(t, got)
}

func TestRedisCache_SetThenGetRoundTrip(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	want := sampleRooms()

	require.NoError(t, c.SetAvailableRooms(ctx, winStart, winEnd, want))

	got, err := c.GetAvailableRooms(ctx, winStart, winEnd)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// Пустой список — валидное значение, а не промах: закэшированный «нет свободных
// комнат» не должен повторно ходить в БД.
func TestRedisCache_EmptyListIsNotMiss(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.SetAvailableRooms(ctx, winStart, winEnd, []model.Room{}))

	got, err := c.GetAvailableRooms(ctx, winStart, winEnd)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestRedisCache_KeyFormatAndTTL(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.SetAvailableRooms(ctx, winStart, winEnd, sampleRooms()))

	key := "rooms:available:" +
		strconv.FormatInt(winStart.Unix(), 10) + ":" + strconv.FormatInt(winEnd.Unix(), 10)
	assert.True(t, mr.Exists(key), "ключ должен иметь формат rooms:available:{start_unix}:{end_unix}")

	// TTL == 5 минут: запись жива до, но не после срока.
	mr.FastForward(cache.TTL - time.Second)
	assert.True(t, mr.Exists(key))
	mr.FastForward(2 * time.Second)
	assert.False(t, mr.Exists(key), "запись должна истечь по TTL")
}

// Инвалидация чистит все окна разом: ключ агрегирует комнаты и не содержит
// room_id, поэтому любое изменение брони сбрасывает весь срез доступности.
func TestRedisCache_InvalidateClearsAllWindows(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	otherStart := winStart.Add(24 * time.Hour)
	otherEnd := otherStart.Add(time.Hour)
	require.NoError(t, c.SetAvailableRooms(ctx, winStart, winEnd, sampleRooms()))
	require.NoError(t, c.SetAvailableRooms(ctx, otherStart, otherEnd, sampleRooms()))

	// Постороннему ключу инвалидация не должна навредить.
	require.NoError(t, mr.Set("unrelated:key", "keep"))

	require.NoError(t, c.InvalidateRoomCache(ctx, "r-1"))

	_, err := c.GetAvailableRooms(ctx, winStart, winEnd)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
	_, err = c.GetAvailableRooms(ctx, otherStart, otherEnd)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
	assert.True(t, mr.Exists("unrelated:key"), "инвалидация трогает только rooms:available:*")
}

func TestRedisCache_InvalidateEmptyIsNoop(t *testing.T) {
	c, _ := newTestCache(t)
	assert.NoError(t, c.InvalidateRoomCache(context.Background(), "r-1"))
}
