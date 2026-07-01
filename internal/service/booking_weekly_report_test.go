package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
)

// bookingStartingAt — бронь комнаты testRoomID с заданным id и началом (длительность 1 час).
func bookingStartingAt(id string, start time.Time) model.Booking {
	return model.Booking{
		ID:        id,
		RoomID:    testRoomID,
		UserID:    testUserID,
		Title:     "meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
		Status:    model.StatusConfirmed,
	}
}

func TestBookingService_GetWeeklyReport(t *testing.T) {
	weekStart := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC) // понедельник
	weekEnd := weekStart.AddDate(0, 0, 7)

	t.Run("groups bookings by start day and always returns 7 days", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		repo := &mockBookingRepo{}
		roomFound(rooms, 2)

		mon0900 := weekStart.Add(9 * time.Hour)
		mon1400 := weekStart.Add(14 * time.Hour)
		wed1000 := weekStart.AddDate(0, 0, 2).Add(10 * time.Hour)
		sun2300 := weekStart.AddDate(0, 0, 6).Add(23 * time.Hour)

		var gotRoomID string
		var gotFrom, gotTo time.Time
		repo.listByRoomFn = func(_ context.Context, roomID string, from, to time.Time) ([]model.Booking, error) {
			gotRoomID, gotFrom, gotTo = roomID, from, to
			return []model.Booking{
				bookingStartingAt("b-mon-1", mon0900),
				bookingStartingAt("b-mon-2", mon1400),
				bookingStartingAt("b-wed", wed1000),
				bookingStartingAt("b-sun", sun2300),
			}, nil
		}

		svc := newTestService(rooms, repo)
		rep, err := svc.GetWeeklyReport(context.Background(), testActor(model.RoleMember), testRoomID, weekStart)
		require.NoError(t, err)

		// Репозиторию передаётся окно [weekStart, weekStart+7d).
		assert.Equal(t, testRoomID, gotRoomID)
		assert.True(t, gotFrom.Equal(weekStart), "from == week start")
		assert.True(t, gotTo.Equal(weekEnd), "to == week end (exclusive)")

		assert.Equal(t, testRoomID, rep.RoomID)
		assert.True(t, rep.WeekStart.Equal(weekStart))
		assert.True(t, rep.WeekEnd.Equal(weekEnd))
		require.Len(t, rep.Days, 7)

		for i, d := range rep.Days {
			assert.True(t, d.Date.Equal(weekStart.AddDate(0, 0, i)), "day %d date", i)
			assert.NotNil(t, d.Bookings, "day %d bookings slice is non-nil", i)
		}
		assert.Len(t, rep.Days[0].Bookings, 2, "monday holds two bookings")
		assert.Len(t, rep.Days[1].Bookings, 0, "tuesday is empty")
		assert.Len(t, rep.Days[2].Bookings, 1, "wednesday holds one booking")
		assert.Len(t, rep.Days[6].Bookings, 1, "sunday holds one booking")
	})

	t.Run("no bookings still yields 7 empty days", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		repo := &mockBookingRepo{}
		roomFound(rooms, 2)
		repo.listByRoomFn = func(_ context.Context, _ string, _, _ time.Time) ([]model.Booking, error) {
			return nil, nil
		}

		svc := newTestService(rooms, repo)
		rep, err := svc.GetWeeklyReport(context.Background(), testActor(model.RoleMember), testRoomID, weekStart)
		require.NoError(t, err)
		require.Len(t, rep.Days, 7)
		for i, d := range rep.Days {
			assert.Empty(t, d.Bookings, "day %d empty", i)
		}
	})

	t.Run("week start is normalized to the start of the UTC day", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		repo := &mockBookingRepo{}
		roomFound(rooms, 2)

		var gotFrom, gotTo time.Time
		repo.listByRoomFn = func(_ context.Context, _ string, from, to time.Time) ([]model.Booking, error) {
			gotFrom, gotTo = from, to
			return nil, nil
		}

		// Полдень в зоне MSK (=09:00 UTC) должно схлопнуться к 2026-05-18 00:00 UTC.
		msk := time.FixedZone("MSK", 3*60*60)
		midday := time.Date(2026, 5, 18, 12, 0, 0, 0, msk)

		svc := newTestService(rooms, repo)
		rep, err := svc.GetWeeklyReport(context.Background(), testActor(model.RoleMember), testRoomID, midday)
		require.NoError(t, err)
		assert.True(t, gotFrom.Equal(weekStart), "normalized to UTC midnight")
		assert.True(t, gotTo.Equal(weekEnd))
		assert.True(t, rep.WeekStart.Equal(weekStart))
	})

	t.Run("empty room id is rejected as validation error", func(t *testing.T) {
		svc := newTestService(&mockRoomLookup{}, &mockBookingRepo{})
		_, err := svc.GetWeeklyReport(context.Background(), testActor(model.RoleMember), "  ", weekStart)
		assert.ErrorAs(t, err, new(*ValidationError))
	})

	t.Run("zero week start is rejected as validation error", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		roomFound(rooms, 2)
		svc := newTestService(rooms, &mockBookingRepo{})
		_, err := svc.GetWeeklyReport(context.Background(), testActor(model.RoleMember), testRoomID, time.Time{})
		assert.ErrorAs(t, err, new(*ValidationError))
	})

	t.Run("missing room maps to ErrRoomNotFound", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
			return model.Room{}, repository.ErrNotFound
		}
		svc := newTestService(rooms, &mockBookingRepo{})
		_, err := svc.GetWeeklyReport(context.Background(), testActor(model.RoleMember), testRoomID, weekStart)
		assert.ErrorIs(t, err, ErrRoomNotFound)
	})

	t.Run("repository error is propagated", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		repo := &mockBookingRepo{}
		roomFound(rooms, 2)
		repo.listByRoomFn = func(_ context.Context, _ string, _, _ time.Time) ([]model.Booking, error) {
			return nil, errAny
		}
		svc := newTestService(rooms, repo)
		_, err := svc.GetWeeklyReport(context.Background(), testActor(model.RoleMember), testRoomID, weekStart)
		assert.ErrorIs(t, err, errAny)
	})
}
