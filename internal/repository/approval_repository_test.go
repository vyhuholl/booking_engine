package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/testutil"
)

// Интеграционные тесты workflow одобрения больших переговорок
// (change add-large-room-approval, разделы 2–3): расширенный предикат занятости и
// репозиторные методы Approve / RejectAndOfferWaitlist / ListPendingApprovals.
// Используют реальный Postgres через testutil.SetupTestDB (нужен Docker).

// seedBookingStatus вставляет бронь заданного статуса и момента создания
// (created_at — якорь таймаута одобрения), возвращает её.
func seedBookingStatus(t *testing.T, pool *pgxpool.Pool, roomID, userID string, start time.Time, status model.BookingStatus, createdAt time.Time) model.Booking {
	t.Helper()
	b := newBooking(roomID, userID, start, time.Hour)
	b.Status = status
	b.CreatedAt = createdAt
	testutil.SeedBooking(t, pool, b)
	return b
}

// TestBooking_OccupancyPredicate: слот удерживают confirmed/pending_approval/approved
// (IsRoomBusy=true, пересекающийся CreateChecked конфликтует), а rejected/cancelled —
// освобождают (IsRoomBusy=false, CreateChecked успешен).
func TestBooking_OccupancyPredicate(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		status   model.BookingStatus
		wantBusy bool
	}{
		{model.StatusConfirmed, true},
		{model.StatusPendingApproval, true},
		{model.StatusApproved, true},
		{model.StatusRejected, false},
		{model.StatusCancelled, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.status), func(t *testing.T) {
			cleanup()
			roomID := seedRoom(t, pool)
			userID := seedUser(t, pool)
			seedBookingStatus(t, pool, roomID, userID, base, tc.status, time.Now())

			busy, err := repo.IsRoomBusy(ctx, roomID, base, base.Add(time.Hour))
			require.NoError(t, err)
			assert.Equal(t, tc.wantBusy, busy, "IsRoomBusy")

			conflict, err := repo.CreateChecked(ctx, newBooking(roomID, userID, base.Add(30*time.Minute), time.Hour))
			require.NoError(t, err)
			if tc.wantBusy {
				assert.NotNil(t, conflict, "overlapping create must conflict")
			} else {
				assert.Nil(t, conflict, "free slot: create must succeed")
			}
		})
	}
}

// TestBooking_Approve: pending_approval → approved; идемпотентность и «не pending»
// дают ErrNotFound (0 строк условного UPDATE).
func TestBooking_Approve(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	now := time.Now()

	t.Run("pending -> approved", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := seedBookingStatus(t, pool, roomID, userID, base, model.StatusPendingApproval, now)

		got, err := repo.Approve(ctx, b.ID, now)
		require.NoError(t, err)
		assert.Equal(t, model.StatusApproved, got.Status)

		reread, err := repo.Get(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, model.StatusApproved, reread.Status)
	})

	t.Run("idempotent: second approve -> ErrNotFound", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := seedBookingStatus(t, pool, roomID, userID, base, model.StatusPendingApproval, now)
		_, err := repo.Approve(ctx, b.ID, now)
		require.NoError(t, err)
		_, err = repo.Approve(ctx, b.ID, now)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})

	t.Run("not pending (confirmed) -> ErrNotFound", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := seedBookingStatus(t, pool, roomID, userID, base, model.StatusConfirmed, now)
		_, err := repo.Approve(ctx, b.ID, now)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})

	t.Run("missing booking -> ErrNotFound", func(t *testing.T) {
		cleanup()
		_, err := repo.Approve(ctx, "bk-missing", now)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}

// TestBooking_RejectAndOfferWaitlist: pending_approval → rejected с сохранённой
// причиной; слот освобождается и предлагается первому в листе ожидания;
// идемпотентность/«не pending» → ErrNotFound.
func TestBooking_RejectAndOfferWaitlist(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	now := time.Now()
	const reason = "room reserved for an event"

	t.Run("pending -> rejected, reason saved, slot freed", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := seedBookingStatus(t, pool, roomID, userID, base, model.StatusPendingApproval, now)

		got, offered, err := repo.RejectAndOfferWaitlist(ctx, b.ID, reason, now)
		require.NoError(t, err)
		assert.Equal(t, model.StatusRejected, got.Status)
		require.NotNil(t, got.RejectionReason)
		assert.Equal(t, reason, *got.RejectionReason)
		assert.Nil(t, offered, "no waitlist -> nothing offered")

		busy, err := repo.IsRoomBusy(ctx, roomID, base, base.Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, busy, "rejected frees the slot")
	})

	t.Run("offers freed slot to first waitlist entry", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		owner := seedUser(t, pool)
		waiter := seedUser(t, pool)
		b := seedBookingStatus(t, pool, roomID, owner, base, model.StatusPendingApproval, now)

		entry := testutil.WaitlistEntry(
			testutil.WithWaitlistRoom(roomID),
			testutil.WithWaitlistUser(waiter),
			testutil.WithWaitlistInterval(base, base.Add(time.Hour)),
			testutil.WithWaitlistStatus(model.WaitlistStatusWaiting),
		)
		testutil.SeedWaitlist(t, pool, entry)

		_, offered, err := repo.RejectAndOfferWaitlist(ctx, b.ID, reason, now)
		require.NoError(t, err)
		require.NotNil(t, offered, "waiting entry must be offered the freed slot")
		assert.Equal(t, entry.ID, offered.ID)
		assert.Equal(t, model.WaitlistStatusOffered, offered.Status)
		require.NotNil(t, offered.OfferedAt)
	})

	t.Run("idempotent: second reject -> ErrNotFound", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := seedBookingStatus(t, pool, roomID, userID, base, model.StatusPendingApproval, now)
		_, _, err := repo.RejectAndOfferWaitlist(ctx, b.ID, reason, now)
		require.NoError(t, err)
		_, _, err = repo.RejectAndOfferWaitlist(ctx, b.ID, reason, now)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})

	t.Run("not pending (approved) -> ErrNotFound", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)
		b := seedBookingStatus(t, pool, roomID, userID, base, model.StatusApproved, now)
		_, _, err := repo.RejectAndOfferWaitlist(ctx, b.ID, reason, now)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}

// TestBooking_Approve_Concurrent: при N одновременных approve одной pending-брони
// ровно один успешен (approved), остальные получают ErrNotFound (0 строк условного
// UPDATE) либо сериализационный сбой — двойного одобрения не происходит.
func TestBooking_Approve_Concurrent(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	now := time.Now()

	cleanup()
	roomID := seedRoom(t, pool)
	userID := seedUser(t, pool)
	b := seedBookingStatus(t, pool, roomID, userID, base, model.StatusPendingApproval, now)

	const attempts = 8
	type outcome struct {
		status model.BookingStatus
		err    error
	}
	results := make([]outcome, attempts)

	var wg sync.WaitGroup
	ready := make(chan struct{}, attempts)
	barrier := make(chan struct{})
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{}
			<-barrier
			got, err := repo.Approve(ctx, b.ID, now)
			results[i] = outcome{status: got.Status, err: err}
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
		case r.err == nil && r.status == model.StatusApproved:
			success++
		case errors.Is(r.err, repository.ErrNotFound):
			notFound++
		case isSerialization(r.err):
			serialization++
		default:
			other = append(other, r.err)
		}
	}
	require.Empty(t, other, "unexpected errors: %v", other)
	assert.Equal(t, 1, success, "exactly one approve succeeds")
	assert.Equal(t, attempts-1, notFound+serialization, "others rejected")

	got, err := repo.Get(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusApproved, got.Status)
}

// TestBooking_ListPendingApprovals: возвращает актуальные pending (по created_at),
// а просроченные (created_at старше timeout) авто-отклоняет и освобождает их слот
// листу ожидания.
func TestBooking_ListPendingApprovals(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewBooking(pool)
	ctx := context.Background()
	base := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	now := time.Now()
	const timeout = 24 * time.Hour
	const reason = "approval timeout exceeded"

	t.Run("returns fresh pending sorted, sweeps expired", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		userID := seedUser(t, pool)

		fresh1 := seedBookingStatus(t, pool, roomID, userID, base, model.StatusPendingApproval, now.Add(-time.Hour))
		fresh2 := seedBookingStatus(t, pool, roomID, userID, base.Add(2*time.Hour), model.StatusPendingApproval, now.Add(-30*time.Minute))
		expired := seedBookingStatus(t, pool, roomID, userID, base.Add(4*time.Hour), model.StatusPendingApproval, now.Add(-25*time.Hour))

		pending, autoRejected, err := repo.ListPendingApprovals(ctx, now, timeout, reason)
		require.NoError(t, err)

		require.Len(t, pending, 2, "only fresh pending returned")
		assert.Equal(t, fresh1.ID, pending[0].ID, "sorted by created_at asc")
		assert.Equal(t, fresh2.ID, pending[1].ID)

		require.Len(t, autoRejected, 1)
		assert.Equal(t, expired.ID, autoRejected[0].ID)
		assert.Equal(t, model.StatusRejected, autoRejected[0].Status)
		require.NotNil(t, autoRejected[0].RejectionReason)
		assert.Equal(t, reason, *autoRejected[0].RejectionReason)

		reread, err := repo.Get(ctx, expired.ID)
		require.NoError(t, err)
		assert.Equal(t, model.StatusRejected, reread.Status, "expired swept in DB")
	})

	t.Run("expired slot offered to waitlist", func(t *testing.T) {
		cleanup()
		roomID := seedRoom(t, pool)
		owner := seedUser(t, pool)
		waiter := seedUser(t, pool)
		seedBookingStatus(t, pool, roomID, owner, base, model.StatusPendingApproval, now.Add(-25*time.Hour))

		entry := testutil.WaitlistEntry(
			testutil.WithWaitlistRoom(roomID),
			testutil.WithWaitlistUser(waiter),
			testutil.WithWaitlistInterval(base, base.Add(time.Hour)),
			testutil.WithWaitlistStatus(model.WaitlistStatusWaiting),
		)
		testutil.SeedWaitlist(t, pool, entry)

		pending, autoRejected, err := repo.ListPendingApprovals(ctx, now, timeout, reason)
		require.NoError(t, err)
		assert.Empty(t, pending)
		require.Len(t, autoRejected, 1)

		e, err := repository.NewWaitlist(pool).Get(ctx, entry.ID)
		require.NoError(t, err)
		assert.Equal(t, model.WaitlistStatusOffered, e.Status, "freed slot offered to waiter")
		require.NotNil(t, e.OfferedAt)
	})
}
