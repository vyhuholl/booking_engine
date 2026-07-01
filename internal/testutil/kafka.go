package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"

	kafkacontainer "github.com/testcontainers/testcontainers-go/modules/kafka"
)

// Один Kafka-контейнер на тестовый бинарник — как и Postgres в database.go.
// Останавливает его Ryuk-reaper testcontainers при завершении процесса.
var (
	kafkaOnce    sync.Once
	kafkaBrokers []string
	kafkaErr     error
)

// StartKafka поднимает общий контейнер Kafka (один раз на бинарник) и возвращает
// адреса брокеров. Не требует *testing.T, поэтому вызывается из TestMain для
// прогрева контейнера до старта тестов (симметрично StartPostgres).
func StartKafka() ([]string, error) {
	kafkaOnce.Do(func() { kafkaBrokers, kafkaErr = bootKafka() })
	return kafkaBrokers, kafkaErr
}

// SetupTestKafka — обёртка StartKafka для тестов: возвращает адреса брокеров
// общего контейнера. Изоляция между тестами достигается уникальным топиком на
// тест (в отличие от таблиц БД или ключей Redis, топик не чистится).
func SetupTestKafka(t testing.TB) []string {
	t.Helper()
	brokers, err := StartKafka()
	if err != nil {
		t.Fatalf("setup test kafka: %v", err)
	}
	return brokers
}

func bootKafka() ([]string, error) {
	ctx := context.Background()
	container, err := kafkacontainer.Run(ctx,
		"confluentinc/confluent-local:7.5.0",
		kafkacontainer.WithClusterID("booking-test"),
	)
	if err != nil {
		return nil, fmt.Errorf("start kafka container: %w", err)
	}
	brokers, err := container.Brokers(ctx)
	if err != nil {
		return nil, fmt.Errorf("kafka brokers: %w", err)
	}
	return brokers, nil
}
