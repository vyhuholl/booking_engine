package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
	"github.com/example/booking-engine/internal/testutil"
)

// TestBookingService_Create_ConcurrentDoubleBooking воспроизводит гонку
// «double booking»: N горутин одновременно пытаются забронировать одну и ту же
// комнату на один и тот же интервал.
//
// Инвариант, который обязан выполняться при любой раскладке планировщика:
//   - в БД оказывается РОВНО одна подтверждённая бронь на интервал;
//   - РОВНО одна попытка Create завершается успехом.
//
// Про «остальные 9 — ErrConflict». Защита от пересечений живёт в
// repository.Booking.CreateChecked, которая работает в транзакции с уровнем
// SERIALIZABLE и НЕ ретраит сериализационные сбои. Поэтому проигравшие попытки
// отклоняются двумя разными путями в зависимости от таймингов:
//   - если их снапшот уже видит закоммиченного победителя — SELECT находит
//     пересечение и сервис возвращает *service.BookingConflictError;
//   - если транзакции реально пересеклись во времени — PostgreSQL SSI откатывает
//     всех, кроме одного, с ошибкой 40001 (serialization_failure), которую
//     CreateChecked пробрасывает как есть.
//
// Оба исхода означают «бронь отклонена», поэтому тест проверяет, что сумма
// (conflict + serialization) равна N-1, а неожиданных ошибок нет. Точная
// разбивка между двумя категориями недетерминирована и выводится через t.Logf.
func TestBookingService_Create_ConcurrentDoubleBooking(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	cleanup() // чистый старт: миграции засевают демо-данные, убираем их

	ctx := context.Background()

	// 1. Setup: одна комната и два пользователя.
	roomID := seedActiveRoom(t, pool)
	user1 := seedMember(t, pool)
	user2 := seedMember(t, pool)

	svc := service.NewBooking(repository.NewRoom(pool), repository.NewBooking(pool), nil, nil, "booking.events", nil)

	// Завтра 10:00–11:00 UTC — гарантированно в будущем для проверки now() в сервисе.
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	end := start.Add(time.Hour)

	in := service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Standup",
		StartTime: start,
		EndTime:   end,
	}

	// Чередуем двух пользователей, чтобы конфликт был межпользовательским.
	actors := []service.Actor{
		{ID: user1, Role: model.RoleMember},
		{ID: user2, Role: model.RoleMember},
	}

	const attempts = 10

	type outcome struct {
		booking model.Booking
		err     error
	}
	results := make([]outcome, attempts)

	// 2–3. Барьер: все горутины встают на receive, main закрывает канал —
	// и они стартуют одновременно. ready даёт гарантию, что все attempts горутин
	// действительно дошли до барьера, до того как он откроется (без time.Sleep).
	var wg sync.WaitGroup
	ready := make(chan struct{}, attempts)
	barrier := make(chan struct{})
	wg.Add(attempts)

	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{} // «я на барьере»
			<-barrier           // общий старт
			b, err := svc.Create(ctx, actors[i%len(actors)], in)
			results[i] = outcome{booking: b, err: err}
		}(i)
	}

	for i := 0; i < attempts; i++ {
		<-ready // дожидаемся, пока все встанут на барьер
	}
	close(barrier) // одновременный старт
	wg.Wait()

	// 4. Собираем результаты.
	var (
		success       int
		conflicts     int
		serialization int
		other         []error
	)
	for _, r := range results {
		switch {
		case r.err == nil:
			success++
			assert.NotEmpty(t, r.booking.ID, "успешная бронь должна иметь id")
		case isConflict(r.err):
			conflicts++
		case isSerializationFailure(r.err):
			serialization++
		default:
			other = append(other, r.err)
		}
	}
	t.Logf("attempts=%d success=%d conflict=%d serialization=%d",
		attempts, success, conflicts, serialization)

	require.Empty(t, other, "неожиданные ошибки (не конфликт и не 40001): %v", other)

	// 5. Проверяем БД: сколько подтверждённых броней реально записано на интервал.
	var stored int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM bookings
		 WHERE room_id = $1 AND status = 'confirmed'
		   AND start_time < $3 AND end_time > $2`,
		roomID, start, end,
	).Scan(&stored))

	// Ожидания.
	assert.Equal(t, 1, stored, "double booking: в БД должна остаться ровно одна бронь на интервал")
	assert.Equal(t, 1, success, "ровно одна попытка должна завершиться успехом")
	assert.Equal(t, attempts-1, conflicts+serialization,
		"остальные попытки должны быть отклонены как conflict либо serialization failure")
}

// isConflict — попытка отклонена сервисом как пересечение брони.
func isConflict(err error) bool {
	var c *service.BookingConflictError
	return errors.As(err, &c)
}

// isSerializationFailure — транзакцию откатил PostgreSQL SSI:
// 40001 serialization_failure или 40P01 deadlock_detected.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

func seedActiveRoom(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := "room-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO rooms (id, name, capacity, floor, equipment, status)
		 VALUES ($1, 'Concurrency Room', 4, 1, '{}', 'active')`, id)
	require.NoError(t, err)
	return id
}

func seedMember(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := "user-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, name, email, role)
		 VALUES ($1, 'Member', $2, 'member')`, id, id+"@example.com")
	require.NoError(t, err)
	return id
}
