// Package testutil содержит вспомогательные функции для интеграционных тестов.
package testutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Один контейнер на тестовый бинарник. Контейнер останавливает Ryuk-reaper
// testcontainers при завершении процесса тестов; cleanup лишь очищает данные.
var (
	sharedOnce sync.Once
	sharedPool *pgxpool.Pool
	sharedErr  error
)

// SetupTestDB поднимает PostgreSQL в testcontainers (один раз на тестовый
// бинарник), применяет миграции из каталога migrations/ и возвращает
// готовый пул соединений вместе с cleanup-функцией.
//
// Cleanup усекает все таблицы public-схемы — это даёт изоляцию состояния
// между тестами при общем контейнере. Рекомендуется вызывать через
// t.Cleanup(cleanup).
func SetupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	pool, err := StartPostgres()
	if err != nil {
		t.Fatalf("setup test db: %v", err)
	}

	cleanup := func() {
		t.Helper()
		if err := truncateAll(context.Background(), pool); err != nil {
			t.Fatalf("truncate test tables: %v", err)
		}
	}
	return pool, cleanup
}

// StartPostgres поднимает общий контейнер Postgres (один раз на тестовый
// бинарник) с применёнными миграциями и возвращает пул. В отличие от SetupTestDB
// не требует *testing.T, поэтому вызывается из TestMain интеграционных наборов,
// чтобы прогреть контейнер до старта тестов. Контейнер живёт до конца процесса —
// его останавливает Ryuk-reaper testcontainers.
func StartPostgres() (*pgxpool.Pool, error) {
	sharedOnce.Do(func() {
		sharedPool, sharedErr = bootPostgres()
	})
	return sharedPool, sharedErr
}

func bootPostgres() (*pgxpool.Pool, error) {
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("booking_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("connection string: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("open pgx pool: %w", err)
	}

	if err := applyMigrations(ctx, pool); err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	return pool, nil
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := migrationsDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", dir, err)
	}
	var ups []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	sort.Strings(ups)

	for _, name := range ups {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("exec %s: %w", name, err)
		}
	}
	return nil
}

// migrationsDir возвращает абсолютный путь к каталогу миграций, опираясь
// на расположение этого исходного файла, а не на рабочий каталог теста.
func migrationsDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve testutil source path")
	}
	// <root>/internal/testutil/database.go -> <root>/migrations
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	return filepath.Join(root, "migrations"), nil
}

func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = 'public'`)
	if err != nil {
		return err
	}
	var quoted []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		quoted = append(quoted, `"`+name+`"`)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(quoted) == 0 {
		return nil
	}
	_, err = pool.Exec(ctx,
		"TRUNCATE TABLE "+strings.Join(quoted, ", ")+" RESTART IDENTITY CASCADE")
	return err
}
