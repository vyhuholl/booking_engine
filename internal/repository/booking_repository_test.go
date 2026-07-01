package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/testutil"
)

// Интеграционные тесты для repository.Booking.
//
// Расхождения с формулировкой задачи (зафиксировано здесь, чтобы не плодить разрозненные комментарии):
//   - В коде нет отдельного метода GetOverlappingBookings. Поиск пересечений выполняется
//     атомарно внутри CreateChecked: при конфликте он возвращает (*model.Booking, nil),
//     при отсутствии — (nil, nil). Эта же связка играет роль "Create + Get overlapping".
//   - У таблицы bookings нет столбца updated_at (см. migrations/001_init.up.sql),
//     поэтому в кейсе отмены проверяется только переход статуса.
//   - repository.Cancel не различает "нет такого id" и "уже отменено" — в обоих случаях
//     возвращается repository.ErrNotFound. Разделение на ErrAlreadyCancelled живёт
//     в сервисном слое (internal/service/errors.go).
//
// Общий setup БД выполняется через testutil.SetupTestDB: внутри него sync.Once
// поднимает один контейнер Postgres на тестовый бинарник и применяет миграции,
// поэтому отдельный TestMain не требуется. cleanup() усекает все таблицы и
// вызывается перед каждым подтестом для изоляции состояния.

func seedRoom(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := "room-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO rooms (id, name, capacity, floor, equipment, status)
		 VALUES ($1, 'Test Room', 4, 1, '{}', 'active')`, id)
	require.NoError(t, err)
	return id
}

func seedUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := "user-" + uuid.NewString()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, name, email, role)
		 VALUES ($1, 'Test User', $2, 'member')`, id, id+"@example.com")
	require.NoError(t, err)
	return id
}

func newBooking(roomID, userID string, start time.Time, dur time.Duration) model.Booking {
	return model.Booking{
		ID:        "bk-" + uuid.NewString(),
		RoomID:    roomID,
		UserID:    userID,
		Title:     "meeting",
		StartTime: start,
		EndTime:   start.Add(dur),
		Status:    model.StatusConfirmed,
	}
}

func TestBooking_CreateChecked(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	t.Run("empty room: single booking succeeds", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		b := newBooking(roomID, userID, base, time.Hour)
		conflict, err := repo.CreateChecked(ctx, b)
		require.NoError(t, err)
		require.Nil(t, conflict)

		got, err := repo.Get(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, b.ID, got.ID)
		assert.Equal(t, model.StatusConfirmed, got.Status)
		assert.True(t, got.StartTime.Equal(b.StartTime))
		assert.True(t, got.EndTime.Equal(b.EndTime))
	})

	t.Run("overlap: existing 10:00-11:00, new 10:30-11:30 — returns existing as conflict", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		existing := newBooking(roomID, userID, base, time.Hour) // 10:00-11:00
		_, err := repo.CreateChecked(ctx, existing)
		require.NoError(t, err)

		newer := newBooking(roomID, userID, base.Add(30*time.Minute), time.Hour) // 10:30-11:30
		conflict, err := repo.CreateChecked(ctx, newer)
		require.NoError(t, err)
		require.NotNil(t, conflict)
		assert.Equal(t, existing.ID, conflict.ID)

		// Вставка не должна была произойти.
		_, err = repo.Get(ctx, newer.ID)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})

	// Граничный случай: 10:00-11:00 и 11:00-12:00.
	// Решение проекта (задача 2.2): интервалы полуоткрытые [start, end), касание границей
	// конфликтом НЕ считается. Это видно по запросу в booking.go:46-47 — строгие неравенства
	// start_time < new.end_time AND end_time > new.start_time: при existing.end == new.start
	// второе условие (11:00 > 11:00) ложно, значит пересечения нет.
	t.Run("boundary touch (10:00-11:00 then 11:00-12:00) is NOT a conflict", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		first := newBooking(roomID, userID, base, time.Hour) // 10:00-11:00
		_, err := repo.CreateChecked(ctx, first)
		require.NoError(t, err)

		second := newBooking(roomID, userID, base.Add(time.Hour), time.Hour) // 11:00-12:00
		conflict, err := repo.CreateChecked(ctx, second)
		require.NoError(t, err)
		assert.Nil(t, conflict, "встык по границе не должно считаться пересечением")

		// Обе записи должны существовать.
		_, err = repo.Get(ctx, first.ID)
		require.NoError(t, err)
		_, err = repo.Get(ctx, second.ID)
		require.NoError(t, err)
	})

	t.Run("partial overlap (10:00-12:00 then 11:00-13:00) conflicts", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		first := newBooking(roomID, userID, base, 2*time.Hour) // 10:00-12:00
		_, err := repo.CreateChecked(ctx, first)
		require.NoError(t, err)

		second := newBooking(roomID, userID, base.Add(time.Hour), 2*time.Hour) // 11:00-13:00
		conflict, err := repo.CreateChecked(ctx, second)
		require.NoError(t, err)
		require.NotNil(t, conflict)
		assert.Equal(t, first.ID, conflict.ID)
	})

	t.Run("inner overlap (10:00-14:00 contains 11:00-13:00) conflicts", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		outer := newBooking(roomID, userID, base, 4*time.Hour) // 10:00-14:00
		_, err := repo.CreateChecked(ctx, outer)
		require.NoError(t, err)

		inner := newBooking(roomID, userID, base.Add(time.Hour), 2*time.Hour) // 11:00-13:00
		conflict, err := repo.CreateChecked(ctx, inner)
		require.NoError(t, err)
		require.NotNil(t, conflict)
		assert.Equal(t, outer.ID, conflict.ID)
	})

	t.Run("cancelled booking is NOT a conflict", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		existing := newBooking(roomID, userID, base, time.Hour)
		_, err := repo.CreateChecked(ctx, existing)
		require.NoError(t, err)
		require.NoError(t, repo.Cancel(ctx, existing.ID))

		newer := newBooking(roomID, userID, base.Add(30*time.Minute), time.Hour)
		conflict, err := repo.CreateChecked(ctx, newer)
		require.NoError(t, err)
		assert.Nil(t, conflict)
	})

	t.Run("different rooms do not conflict on identical interval", func(t *testing.T) {
		cleanup()
		roomA := seedRoom(t, pool)
		roomB := seedRoom(t, pool)
		userID := seedUser(t, pool)

		a := newBooking(roomA, userID, base, time.Hour)
		_, err := repo.CreateChecked(ctx, a)
		require.NoError(t, err)

		b := newBooking(roomB, userID, base, time.Hour)
		conflict, err := repo.CreateChecked(ctx, b)
		require.NoError(t, err)
		assert.Nil(t, conflict)
	})
}

func TestBooking_CountByRoomInPeriod(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	// now имитируем как фиксированный момент; окно "последнего месяца" — [from, to).
	to := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	from := to.AddDate(0, -1, 0) // 2030-05-01 12:00

	t.Run("counts only bookings whose start_time falls inside [from, to)", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		otherRoom := seedRoom(t, pool)
		userID := seedUser(t, pool)

		// В окне: две брони.
		inWindowA := newBooking(roomID, userID, from.Add(24*time.Hour), time.Hour)
		inWindowB := newBooking(roomID, userID, to.Add(-24*time.Hour), time.Hour)
		// Вне окна: раньше from и позже/на границе to.
		beforeWindow := newBooking(roomID, userID, from.Add(-time.Hour), time.Hour)
		atUpperBound := newBooking(roomID, userID, to, time.Hour) // to исключается (полуоткрытый интервал)
		// Другая комната — не считается.
		otherRoomBooking := newBooking(otherRoom, userID, from.Add(48*time.Hour), time.Hour)

		for _, b := range []model.Booking{inWindowA, inWindowB, beforeWindow, atUpperBound, otherRoomBooking} {
			_, err := repo.CreateChecked(ctx, b)
			require.NoError(t, err)
		}

		n, err := repo.CountByRoomInPeriod(ctx, roomID, from, to)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
	})

	t.Run("cancelled bookings are also counted", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		confirmed := newBooking(roomID, userID, from.Add(24*time.Hour), time.Hour)
		cancelled := newBooking(roomID, userID, from.Add(48*time.Hour), time.Hour)
		_, err := repo.CreateChecked(ctx, confirmed)
		require.NoError(t, err)
		_, err = repo.CreateChecked(ctx, cancelled)
		require.NoError(t, err)
		require.NoError(t, repo.Cancel(ctx, cancelled.ID))

		n, err := repo.CountByRoomInPeriod(ctx, roomID, from, to)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
	})

	t.Run("room without bookings in the period returns zero", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)

		n, err := repo.CountByRoomInPeriod(ctx, roomID, from, to)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
}

func TestBooking_ListByRoomInPeriod(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	from := time.Date(2030, 5, 20, 0, 0, 0, 0, time.UTC) // понедельник
	to := from.AddDate(0, 0, 7)

	t.Run("returns room's bookings with start in [from, to) ordered by start_time", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		otherRoom := seedRoom(t, pool)
		userID := seedUser(t, pool)

		// В окне (вставляем не по порядку, чтобы проверить ORDER BY start_time).
		wed := newBooking(roomID, userID, from.AddDate(0, 0, 2).Add(10*time.Hour), time.Hour)
		mon := newBooking(roomID, userID, from.Add(9*time.Hour), time.Hour)
		// Границы полуоткрытого интервала.
		before := newBooking(roomID, userID, from.Add(-time.Hour), time.Hour)
		atUpper := newBooking(roomID, userID, to, time.Hour) // to исключается
		// Другая комната — не попадает в выборку.
		other := newBooking(otherRoom, userID, from.Add(48*time.Hour), time.Hour)

		for _, b := range []model.Booking{wed, mon, before, atUpper, other} {
			_, err := repo.CreateChecked(ctx, b)
			require.NoError(t, err)
		}

		got, err := repo.ListByRoomInPeriod(ctx, roomID, from, to)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, mon.ID, got[0].ID, "ordered by start_time ascending")
		assert.Equal(t, wed.ID, got[1].ID)
	})

	t.Run("cancelled bookings are included", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		b := newBooking(roomID, userID, from.Add(24*time.Hour), time.Hour)
		_, err := repo.CreateChecked(ctx, b)
		require.NoError(t, err)
		require.NoError(t, repo.Cancel(ctx, b.ID))

		got, err := repo.ListByRoomInPeriod(ctx, roomID, from, to)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, model.StatusCancelled, got[0].Status)
	})

	t.Run("room without bookings in the period returns empty", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)

		got, err := repo.ListByRoomInPeriod(ctx, roomID, from, to)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestBooking_Cancel(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	t.Run("cancel existing confirmed booking flips status to cancelled", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := newBooking(roomID, userID, base, time.Hour)
		_, err := repo.CreateChecked(ctx, b)
		require.NoError(t, err)

		require.NoError(t, repo.Cancel(ctx, b.ID))

		got, err := repo.Get(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, model.StatusCancelled, got.Status)
	})

	t.Run("cancel non-existent returns ErrNotFound", func(t *testing.T) {
		cleanup()
		err := repo.Cancel(ctx, "does-not-exist")
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})

	// На уровне репозитория повторная отмена эквивалентна "нет записи в статусе confirmed":
	// SQL UPDATE с WHERE status = 'confirmed' даёт 0 rows affected → ErrNotFound.
	// Разделение на ErrAlreadyCancelled выполняется в service.Booking.Cancel.
	t.Run("re-cancel of already cancelled booking returns ErrNotFound", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := newBooking(roomID, userID, base, time.Hour)
		_, err := repo.CreateChecked(ctx, b)
		require.NoError(t, err)
		require.NoError(t, repo.Cancel(ctx, b.ID))

		err = repo.Cancel(ctx, b.ID)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}
