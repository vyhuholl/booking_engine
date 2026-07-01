package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	rediscontainer "github.com/testcontainers/testcontainers-go/modules/redis"
)

// Один Redis-контейнер на тестовый бинарник (как Postgres и Kafka). Данные между
// тестами изолирует cleanup из SetupTestRedis (FLUSHALL), сам контейнер снимает
// Ryuk-reaper при завершении процесса.
var (
	redisOnce sync.Once
	redisAddr string
	redisErr  error
)

// StartRedis поднимает общий контейнер Redis (один раз на бинарник) и возвращает
// его адрес host:port. Не требует *testing.T — вызывается из TestMain для
// прогрева контейнера до старта тестов (симметрично StartPostgres/StartKafka).
func StartRedis() (string, error) {
	redisOnce.Do(func() { redisAddr, redisErr = bootRedis() })
	return redisAddr, redisErr
}

// SetupTestRedis возвращает клиент к общему контейнеру Redis и cleanup, делающий
// FLUSHALL (изоляция данных между тестами — аналог truncate для БД). Сам клиент
// закрывается автоматически через t.Cleanup.
func SetupTestRedis(t testing.TB) (*redis.Client, func()) {
	t.Helper()
	addr, err := StartRedis()
	if err != nil {
		t.Fatalf("setup test redis: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })

	cleanup := func() {
		if err := client.FlushAll(context.Background()).Err(); err != nil {
			t.Fatalf("flush redis: %v", err)
		}
	}
	return client, cleanup
}

func bootRedis() (string, error) {
	ctx := context.Background()
	container, err := rediscontainer.Run(ctx, "redis:7-alpine")
	if err != nil {
		return "", fmt.Errorf("start redis container: %w", err)
	}
	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		return "", fmt.Errorf("redis connection string: %w", err)
	}
	opts, err := redis.ParseURL(connStr)
	if err != nil {
		return "", fmt.Errorf("parse redis url %q: %w", connStr, err)
	}
	return opts.Addr, nil
}
