package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound — нормализованная ошибка "запись отсутствует".
var ErrNotFound = errors.New("not found")

// ErrConflict — нормализованная ошибка нарушения уникальности (23505).
var ErrConflict = errors.New("conflict")

// ErrNoOverlap — факт данных: нет подтверждённой брони, пересекающей интервал.
// Сервис интерпретирует его как «комната свободна» (лист ожидания не нужен).
var ErrNoOverlap = errors.New("no overlapping confirmed booking")

// errRollback — внутренний сигнал закрытия транзакции штатным откатом (не сбой):
// runSerializable откатывает транзакцию, не ретраит и возвращает эту ошибку
// вызывающему методу, который трактует её как «результат получен, писать не нужно»
// (например, обнаружен конфликт брони).
var errRollback = errors.New("intentional rollback")

// serializationMaxAttempts — сколько раз runSerializable повторяет транзакцию при
// сериализационном сбое (40001) или дедлоке (40P01), прежде чем сдаться.
const serializationMaxAttempts = 10

func wrapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// isUniqueViolation сообщает, что ошибка — нарушение unique-ограничения PostgreSQL (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isSerializationFailure сообщает, что транзакцию откатил PostgreSQL SSI
// (40001 serialization_failure) или обнаружен дедлок (40P01) — оба безопасно
// повторить, перезапустив всю транзакцию.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// runSerializable выполняет fn в транзакции уровня Serializable, повторяя её при
// сериализационных сбоях (до serializationMaxAttempts раз). fn не должна коммитить
// или откатывать транзакцию — это делает обёртка. Значения-результаты fn прокидывает
// через замыкание и обязана переинициализировать их в начале каждой попытки
// (fn может быть вызвана несколько раз). Возврат errRollback из fn означает штатный
// откат без ретрая — обёртка вернёт errRollback как есть.
func runSerializable(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < serializationMaxAttempts; attempt++ {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback(ctx)
			if isSerializationFailure(err) {
				lastErr = err
				continue
			}
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx) // no-op при уже закрытой транзакции
			if isSerializationFailure(err) {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return lastErr
}
