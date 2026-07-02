package repository_test

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
	"github.com/example/booking-engine/internal/testutil"
)

// Интеграционные тесты для repository.Waitlist и связки отмены брони с предложением
// слота (repository.Booking.CancelAndOfferWaitlist). Нужен Docker (см. testutil.SetupTestDB).

func newWaitlistEntry(roomID, userID string, start time.Time, dur time.Duration) model.WaitlistEntry {
	return model.WaitlistEntry{
		ID:        "wl-" + uuid.NewString(),
		RoomID:    roomID,
		UserID:    userID,
		StartTime: start,
		EndTime:   start.Add(dur),
		Status:    model.WaitlistStatusWaiting,
		CreatedAt: start.Add(-time.Hour),
	}
}

// seedBusy засевает подтверждённую бронь, делающую комнату занятой на [start, start+dur).
// Без неё Waitlist.Create отклоняет запись как ErrNoOverlap (в очередь можно вставать
// только на занятый интервал — проверка теперь атомарна с вставкой).
func seedBusy(t *testing.T, pool *pgxpool.Pool, roomID string, start time.Time, dur time.Duration) {
	t.Helper()
	owner := seedUser(t, pool)
	testutil.SeedBooking(t, pool, newBooking(roomID, owner, start, dur))
}

func TestWaitlist_Create_PositionAndUniqueness(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewWaitlist(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	t.Run("position auto-increments per room", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		seedBusy(t, pool, roomID, base, time.Hour)
		u1, u2 := seedUser(t, pool), seedUser(t, pool)

		e1, err := repo.Create(ctx, newWaitlistEntry(roomID, u1, base, time.Hour))
		require.NoError(t, err)
		assert.Equal(t, 1, e1.Position)

		e2, err := repo.Create(ctx, newWaitlistEntry(roomID, u2, base, time.Hour))
		require.NoError(t, err)
		assert.Equal(t, 2, e2.Position)
	})

	t.Run("duplicate active entry rejected", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		seedBusy(t, pool, roomID, base, time.Hour)
		u1 := seedUser(t, pool)

		_, err := repo.Create(ctx, newWaitlistEntry(roomID, u1, base, time.Hour))
		require.NoError(t, err)

		// Тот же пользователь, комната и интервал → нарушение uq_waitlist_active.
		_, err = repo.Create(ctx, newWaitlistEntry(roomID, u1, base, time.Hour))
		assert.ErrorIs(t, err, repository.ErrConflict)
	})

	t.Run("Get returns stored entry", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		seedBusy(t, pool, roomID, base, time.Hour)
		u1 := seedUser(t, pool)

		created, err := repo.Create(ctx, newWaitlistEntry(roomID, u1, base, time.Hour))
		require.NoError(t, err)

		got, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, created.ID, got.ID)
		assert.Equal(t, model.WaitlistStatusWaiting, got.Status)
		assert.Equal(t, 1, got.Position)
		assert.Nil(t, got.OfferedAt)
	})

	t.Run("Get missing returns ErrNotFound", func(t *testing.T) {
		cleanup()
		_, err := repo.Get(ctx, "wl-ghost")
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}

func TestWaitlist_ListByRoom_OrderedByPosition(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewWaitlist(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	cleanup()
	roomID := seedRoom(t, pool)
	seedBusy(t, pool, roomID, base, time.Hour)

	for i := 0; i < 3; i++ {
		u := seedUser(t, pool)
		_, err := repo.Create(ctx, newWaitlistEntry(roomID, u, base, time.Hour))
		require.NoError(t, err)
	}

	got, err := repo.ListByRoom(ctx, roomID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{got[0].Position, got[1].Position, got[2].Position})

	// Пустая очередь — пустой срез, не nil.
	empty, err := repo.ListByRoom(ctx, "room-none")
	require.NoError(t, err)
	assert.Empty(t, empty)
	assert.NotNil(t, empty)
}

func TestWaitlist_Delete(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewWaitlist(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	cleanup()
	roomID := seedRoom(t, pool)
	seedBusy(t, pool, roomID, base, time.Hour)
	u := seedUser(t, pool)
	created, err := repo.Create(ctx, newWaitlistEntry(roomID, u, base, time.Hour))
	require.NoError(t, err)

	require.NoError(t, repo.Delete(ctx, created.ID))

	_, err = repo.Get(ctx, created.ID)
	assert.ErrorIs(t, err, repository.ErrNotFound)

	// Повторное удаление — ErrNotFound.
	assert.ErrorIs(t, repo.Delete(ctx, created.ID), repository.ErrNotFound)
}

// TestWaitlist_Create_RejectsFreeRoom: встать в очередь на свободный интервал нельзя —
// проверка занятости атомарна со вставкой (см. ErrNoOverlap).
func TestWaitlist_Create_RejectsFreeRoom(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewWaitlist(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	cleanup()
	roomID := seedRoom(t, pool)
	u := seedUser(t, pool)

	_, err := repo.Create(ctx, newWaitlistEntry(roomID, u, base, time.Hour))
	assert.ErrorIs(t, err, repository.ErrNoOverlap)
}

func TestWaitlist_DeleteAndOfferNext(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewWaitlist(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	now := base.Add(-30 * time.Minute)

	t.Run("deleting offered entry offers next by position", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		u1, u2 := seedUser(t, pool), seedUser(t, pool)

		w1 := testutil.WaitlistEntry(
			testutil.WithWaitlistID("wl-"+uuid.NewString()),
			testutil.WithWaitlistRoom(roomID), testutil.WithWaitlistUser(u1),
			testutil.WithWaitlistInterval(base, base.Add(time.Hour)),
			testutil.WithWaitlistPosition(1),
			testutil.WithOfferedAt(base.Add(-40*time.Minute)),
		)
		w2 := testutil.WaitlistEntry(
			testutil.WithWaitlistID("wl-"+uuid.NewString()),
			testutil.WithWaitlistRoom(roomID), testutil.WithWaitlistUser(u2),
			testutil.WithWaitlistInterval(base, base.Add(time.Hour)),
			testutil.WithWaitlistPosition(2),
		)
		testutil.SeedWaitlist(t, pool, w1)
		testutil.SeedWaitlist(t, pool, w2)

		offered, err := repo.DeleteAndOfferNext(ctx, w1.ID, now)
		require.NoError(t, err)
		require.NotNil(t, offered)
		assert.Equal(t, w2.ID, offered.ID)
		assert.Equal(t, model.WaitlistStatusOffered, offered.Status)

		_, err = repo.Get(ctx, w1.ID)
		assert.ErrorIs(t, err, repository.ErrNotFound, "снятая offered-запись удалена")

		got2, err := repo.Get(ctx, w2.ID)
		require.NoError(t, err)
		assert.Equal(t, model.WaitlistStatusOffered, got2.Status, "следующий получил слот")
		require.NotNil(t, got2.OfferedAt)
		assert.True(t, got2.OfferedAt.Equal(now))
	})

	t.Run("deleting waiting entry offers nothing", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		u := seedUser(t, pool)
		w := testutil.WaitlistEntry(
			testutil.WithWaitlistID("wl-"+uuid.NewString()),
			testutil.WithWaitlistRoom(roomID), testutil.WithWaitlistUser(u),
			testutil.WithWaitlistInterval(base, base.Add(time.Hour)),
		)
		testutil.SeedWaitlist(t, pool, w)

		offered, err := repo.DeleteAndOfferNext(ctx, w.ID, now)
		require.NoError(t, err)
		assert.Nil(t, offered, "ждавшая запись слот не держала — предлагать нечего")
	})

	t.Run("missing entry returns ErrNotFound", func(t *testing.T) {
		cleanup()
		_, err := repo.DeleteAndOfferNext(ctx, "wl-ghost", now)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}

func TestBooking_CancelAndOfferWaitlist(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	bookingRepo := repository.NewBooking(pool)
	wlRepo := repository.NewWaitlist(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	now := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)

	t.Run("offers first waiting entry by position", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		owner := seedUser(t, pool)
		u1, u2 := seedUser(t, pool), seedUser(t, pool)

		b := newBooking(roomID, owner, base, time.Hour)
		_, err := bookingRepo.CreateChecked(ctx, b)
		require.NoError(t, err)

		e1, err := wlRepo.Create(ctx, newWaitlistEntry(roomID, u1, base, time.Hour))
		require.NoError(t, err)
		e2, err := wlRepo.Create(ctx, newWaitlistEntry(roomID, u2, base, time.Hour))
		require.NoError(t, err)

		cancelled, offered, err := bookingRepo.CancelAndOfferWaitlist(ctx, b.ID, now)
		require.NoError(t, err)
		assert.Equal(t, model.StatusCancelled, cancelled.Status)
		require.NotNil(t, offered)
		assert.Equal(t, e1.ID, offered.ID, "первым (position 1) предлагается e1")
		assert.Equal(t, model.WaitlistStatusOffered, offered.Status)
		require.NotNil(t, offered.OfferedAt)
		assert.True(t, offered.OfferedAt.Equal(now))

		// e2 остаётся waiting.
		got2, err := wlRepo.Get(ctx, e2.ID)
		require.NoError(t, err)
		assert.Equal(t, model.WaitlistStatusWaiting, got2.Status)
	})

	t.Run("no waiting entries: cancel succeeds, nothing offered", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		owner := seedUser(t, pool)

		b := newBooking(roomID, owner, base, time.Hour)
		_, err := bookingRepo.CreateChecked(ctx, b)
		require.NoError(t, err)

		cancelled, offered, err := bookingRepo.CancelAndOfferWaitlist(ctx, b.ID, now)
		require.NoError(t, err)
		assert.Equal(t, model.StatusCancelled, cancelled.Status)
		assert.Nil(t, offered)
	})

	t.Run("already cancelled: ErrNotFound", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		owner := seedUser(t, pool)

		b := newBooking(roomID, owner, base, time.Hour)
		_, err := bookingRepo.CreateChecked(ctx, b)
		require.NoError(t, err)
		require.NoError(t, bookingRepo.Cancel(ctx, b.ID))

		_, _, err = bookingRepo.CancelAndOfferWaitlist(ctx, b.ID, now)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}

func TestBooking_IsRoomBusy(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	cleanup()
	roomID := seedRoom(t, pool)
	owner := seedUser(t, pool)
	b := newBooking(roomID, owner, base, time.Hour)
	_, err := repo.CreateChecked(ctx, b)
	require.NoError(t, err)

	busy, err := repo.IsRoomBusy(ctx, roomID, base.Add(30*time.Minute), base.Add(90*time.Minute))
	require.NoError(t, err)
	assert.True(t, busy, "пересекающийся интервал — занято")

	free, err := repo.IsRoomBusy(ctx, roomID, base.Add(2*time.Hour), base.Add(3*time.Hour))
	require.NoError(t, err)
	assert.False(t, free, "непересекающийся интервал — свободно")
}

// TestWaitlist_ConfirmAndBook_Concurrent воспроизводит гонку двух подтверждений
// одного offered-слота: N горутин одновременно вызывают ConfirmAndBook на одну
// запись. Инвариант: ровно один успех, в БД ровно одна бронь на интервал; запись
// становится converted. Проигравшие отклоняются как ErrNotFound (условный UPDATE
// увидел уже не offered) либо как serialization_failure (SSI-откат).
func TestWaitlist_ConfirmAndBook_Concurrent(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewWaitlist(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	cleanup()
	roomID := seedRoom(t, pool)
	owner := seedUser(t, pool)

	// Заранее offered-запись на интервал [base, base+1h).
	entry := testutil.WaitlistEntry(
		testutil.WithWaitlistID("wl-"+uuid.NewString()),
		testutil.WithWaitlistRoom(roomID),
		testutil.WithWaitlistUser(owner),
		testutil.WithWaitlistInterval(base, base.Add(time.Hour)),
		testutil.WithOfferedAt(base.Add(-30*time.Minute)),
	)
	testutil.SeedWaitlist(t, pool, entry)

	const attempts = 8
	type outcome struct {
		booking *model.Booking
		err     error
	}
	results := make([]outcome, attempts)

	var wg sync.WaitGroup
	ready := make(chan struct{}, attempts)
	barrier := make(chan struct{})
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			b := newBooking(roomID, owner, base, time.Hour)
			ready <- struct{}{}
			<-barrier
			conflict, err := repo.ConfirmAndBook(ctx, entry.ID, b)
			results[i] = outcome{booking: conflict, err: err}
		}(i)
	}
	for i := 0; i < attempts; i++ {
		<-ready
	}
	close(barrier)
	wg.Wait()

	var success, notFound, serialization int
	var other []error
	for _, r := range results {
		switch {
		case r.err == nil && r.booking == nil:
			success++
		case errors.Is(r.err, repository.ErrNotFound):
			notFound++
		case isSerialization(r.err):
			serialization++
		default:
			other = append(other, r.err)
		}
	}
	require.Empty(t, other, "неожиданные ошибки: %v", other)
	assert.Equal(t, 1, success, "ровно одно подтверждение успешно")
	assert.Equal(t, attempts-1, notFound+serialization, "остальные отклонены")

	// В БД ровно одна бронь на интервал, запись — converted.
	var bookings int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM bookings
		 WHERE room_id = $1 AND status = 'confirmed'
		   AND start_time < $3 AND end_time > $2`,
		roomID, base, base.Add(time.Hour),
	).Scan(&bookings))
	assert.Equal(t, 1, bookings)

	got, err := repo.Get(ctx, entry.ID)
	require.NoError(t, err)
	assert.Equal(t, model.WaitlistStatusConverted, got.Status)
}

// isSerialization — транзакцию откатил PostgreSQL SSI (40001) или дедлок (40P01).
func isSerialization(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}
