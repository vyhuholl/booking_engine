package cache_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/cache"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
	"github.com/example/booking-engine/internal/testutil"
)

// Интеграционные тесты кэша поверх реального Redis (Docker), в дополнение к
// Docker-free проверкам на miniredis в redis_test.go. Общие фикстуры winStart/
// winEnd/sampleRooms берём оттуда же — оба файла в пакете cache_test.
//
// TestMain поднимает контейнеры набора (Redis — сам кэш, Postgres — БД для
// проверки фолбэка сервиса) один раз до старта тестов через sync.Once в testutil;
// Ryuk-reaper снимает их при выходе процесса. Без Docker интеграционные тесты
// пропускаются (requireContainers), а тесты на miniredis выполняются.

var integrationContainersErr error

func TestMain(m *testing.M) {
	if _, err := testutil.StartRedis(); err != nil {
		integrationContainersErr = err
	} else if _, err := testutil.StartPostgres(); err != nil {
		integrationContainersErr = err
	}
	os.Exit(m.Run())
}

func requireContainers(t *testing.T) {
	t.Helper()
	if integrationContainersErr != nil {
		t.Skipf("integration containers unavailable (Docker required): %v", integrationContainersErr)
	}
}

func TestRoomCache_SetThenGet_ReturnsFromCache(t *testing.T) {
	requireContainers(t)
	client, cleanup := testutil.SetupTestRedis(t)
	cleanup()

	c := cache.NewRedis(client, nil)
	ctx := context.Background()
	want := sampleRooms()

	require.NoError(t, c.SetAvailableRooms(ctx, winStart, winEnd, want))

	got, err := c.GetAvailableRooms(ctx, winStart, winEnd)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestRoomCache_ExpiresAfterTTL проверяет истечение записи на реальном Redis:
// с маленьким TTL после ожидания GetAvailableRooms промахивается.
func TestRoomCache_ExpiresAfterTTL(t *testing.T) {
	requireContainers(t)
	client, cleanup := testutil.SetupTestRedis(t)
	cleanup()

	c := cache.NewRedis(client, nil, cache.WithTTL(time.Second))
	ctx := context.Background()
	require.NoError(t, c.SetAvailableRooms(ctx, winStart, winEnd, sampleRooms()))

	// Сразу после записи — попадание.
	_, err := c.GetAvailableRooms(ctx, winStart, winEnd)
	require.NoError(t, err)

	// После TTL Redis удаляет ключ — промах.
	time.Sleep(1500 * time.Millisecond)
	_, err = c.GetAvailableRooms(ctx, winStart, winEnd)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
}

func TestRoomCache_InvalidateRemovesKeys(t *testing.T) {
	requireContainers(t)
	client, cleanup := testutil.SetupTestRedis(t)
	cleanup()

	c := cache.NewRedis(client, nil)
	ctx := context.Background()
	otherStart := winStart.Add(24 * time.Hour)
	otherEnd := otherStart.Add(time.Hour)

	require.NoError(t, c.SetAvailableRooms(ctx, winStart, winEnd, sampleRooms()))
	require.NoError(t, c.SetAvailableRooms(ctx, otherStart, otherEnd, sampleRooms()))
	// Посторонний ключ инвалидация трогать не должна.
	require.NoError(t, client.Set(ctx, "unrelated:key", "keep", 0).Err())

	require.NoError(t, c.InvalidateRoomCache(ctx, "r-1"))

	_, err := c.GetAvailableRooms(ctx, winStart, winEnd)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
	_, err = c.GetAvailableRooms(ctx, otherStart, otherEnd)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)

	exists, err := client.Exists(ctx, "unrelated:key").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists, "инвалидация трогает только rooms:available:*")
}

// TestGetAvailableRooms_RedisDown_FallsBackToDB фиксирует деградацию сервиса:
// при недоступном Redis GetAvailableRooms не падает, а идёт в БД.
func TestGetAvailableRooms_RedisDown_FallsBackToDB(t *testing.T) {
	requireContainers(t)
	pool, cleanup := testutil.SetupTestDB(t)
	cleanup()
	roomID := testutil.SeedRoom(t, pool)

	// Кэш смотрит на мёртвый Redis: любой запрос завершается ошибкой (не промахом),
	// сервис обязан деградировать к БД. Без ретраев и с коротким дедлайном, чтобы
	// тест не висел на недоступном адресе.
	dead := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 250 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = dead.Close() })

	logger, logs := testutil.CaptureLogger()
	roomCache := cache.NewRedis(dead, logger)
	svc := service.NewBooking(repository.NewRoom(pool), repository.NewBooking(pool), roomCache, nil, "", logger)

	start := time.Now().Add(2 * time.Hour)
	rooms, err := svc.GetAvailableRooms(
		context.Background(),
		service.Actor{ID: "user-x", Role: model.RoleMember},
		start, start.Add(time.Hour),
	)
	require.NoError(t, err) // деградация к БД — запрос не роняем
	require.Len(t, rooms, 1)
	assert.Equal(t, roomID, rooms[0].ID)

	// Обращение к недоступному кэшу залогировано как фолбэк.
	assert.Contains(t, logs.String(), "falling back to db")
}
