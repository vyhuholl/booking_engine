package testutil

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
)

func TestRoomBuilder(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		r := Room()
		assert.Equal(t, RoomID, r.ID)
		assert.Equal(t, 6, r.Capacity)
		assert.Equal(t, 2, r.Floor)
		assert.Equal(t, model.RoomStatusActive, r.Status)
		assert.Empty(t, r.Equipment)
	})

	t.Run("options override in order", func(t *testing.T) {
		r := Room(
			WithRoomID("room-x"),
			WithFloor(5),
			WithCapacity(12),
			WithRoomStatus(model.RoomStatusOutOfService),
			WithEquipment(model.EquipmentProjector, model.EquipmentWhiteboard),
		)
		assert.Equal(t, "room-x", r.ID)
		assert.Equal(t, 5, r.Floor)
		assert.Equal(t, 12, r.Capacity)
		assert.Equal(t, model.RoomStatusOutOfService, r.Status)
		assert.Equal(t, []model.Equipment{model.EquipmentProjector, model.EquipmentWhiteboard}, r.Equipment)
	})
}

func TestUserBuilder(t *testing.T) {
	t.Run("defaults are a plain member", func(t *testing.T) {
		u := User()
		assert.Equal(t, UserID, u.ID)
		assert.Equal(t, model.RoleMember, u.Role)
		assert.Nil(t, u.ManagesFloor)
	})

	t.Run("manager with floor", func(t *testing.T) {
		u := User(WithUserID("u-2"), WithRole(model.RoleManager), WithManagesFloor(3), WithEmail("m@example.com"))
		assert.Equal(t, "u-2", u.ID)
		assert.Equal(t, model.RoleManager, u.Role)
		assert.Equal(t, "m@example.com", u.Email)
		if assert.NotNil(t, u.ManagesFloor) {
			assert.Equal(t, 3, *u.ManagesFloor)
		}
	})

	t.Run("each WithManagesFloor captures its own value", func(t *testing.T) {
		a := User(WithManagesFloor(1))
		b := User(WithManagesFloor(2))
		require.NotNil(t, a.ManagesFloor)
		require.NotNil(t, b.ManagesFloor)
		assert.Equal(t, 1, *a.ManagesFloor)
		assert.Equal(t, 2, *b.ManagesFloor)
	})
}

func TestBookingBuilder(t *testing.T) {
	t.Run("defaults: confirmed one-hour booking", func(t *testing.T) {
		b := Booking()
		assert.Equal(t, BookingID, b.ID)
		assert.Equal(t, RoomID, b.RoomID)
		assert.Equal(t, UserID, b.UserID)
		assert.Equal(t, model.StatusConfirmed, b.Status)
		assert.True(t, b.StartTime.Equal(BaseStart))
		assert.Equal(t, time.Hour, b.EndTime.Sub(b.StartTime))
	})

	t.Run("WithStart resets end to start+1h", func(t *testing.T) {
		start := BaseStart.Add(3 * time.Hour)
		b := Booking(WithStart(start))
		assert.True(t, b.StartTime.Equal(start))
		assert.True(t, b.EndTime.Equal(start.Add(time.Hour)))
	})

	t.Run("WithDuration after WithStart composes", func(t *testing.T) {
		start := BaseStart.Add(3 * time.Hour)
		b := Booking(WithStart(start), WithDuration(2*time.Hour))
		assert.True(t, b.StartTime.Equal(start))
		assert.True(t, b.EndTime.Equal(start.Add(2*time.Hour)))
	})

	t.Run("WithInterval sets both ends explicitly", func(t *testing.T) {
		b := Booking(WithInterval(BaseStart, BaseEnd))
		assert.True(t, b.StartTime.Equal(BaseStart))
		assert.True(t, b.EndTime.Equal(BaseEnd))
	})

	t.Run("owner/room/status/title overrides", func(t *testing.T) {
		b := Booking(
			WithBookingID("b-9"),
			WithRoom("room-9"),
			WithOwner(OtherUserID),
			WithBookingStatus(model.StatusCancelled),
			WithTitle("Retro"),
		)
		assert.Equal(t, "b-9", b.ID)
		assert.Equal(t, "room-9", b.RoomID)
		assert.Equal(t, OtherUserID, b.UserID)
		assert.Equal(t, model.StatusCancelled, b.Status)
		assert.Equal(t, "Retro", b.Title)
	})
}

func TestClock(t *testing.T) {
	now := Clock(FixedNow)
	assert.True(t, now().Equal(FixedNow))
	assert.True(t, now().Equal(FixedNow), "stable across calls")
}

// --- Asserts (happy paths) ----------------------------------------------

type validationErr struct{ field string }

func (e *validationErr) Error() string { return "invalid " + e.field }

func TestAssertHelpers(t *testing.T) {
	sentinel := errors.New("boom")

	t.Run("AssertServiceError matches sentinel", func(t *testing.T) {
		AssertServiceError(t, fmt.Errorf("wrap: %w", sentinel), sentinel, nil)
	})

	t.Run("AssertServiceError matches typed", func(t *testing.T) {
		AssertServiceError(t, &validationErr{field: "title"}, nil, new(*validationErr))
	})

	t.Run("AssertSentinel matches wrapped", func(t *testing.T) {
		AssertSentinel(t, fmt.Errorf("wrap: %w", sentinel), sentinel)
	})

	t.Run("AssertTyped returns the matched error", func(t *testing.T) {
		got := AssertTyped[*validationErr](t, fmt.Errorf("wrap: %w", &validationErr{field: "name"}))
		require.NotNil(t, got)
		assert.Equal(t, "name", got.field)
	})
}
