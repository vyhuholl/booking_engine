package testutil

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/model"
)

func TestNewBookingBuilder(t *testing.T) {
	t.Run("defaults: confirmed one-hour booking on default room/user", func(t *testing.T) {
		b := NewBookingBuilder(t).Build()

		assert.True(t, strings.HasPrefix(b.ID, "b-"), "id should carry b- prefix, got %q", b.ID)
		assert.Equal(t, RoomID, b.RoomID)
		assert.Equal(t, UserID, b.UserID)
		assert.Equal(t, model.StatusConfirmed, b.Status)
		assert.True(t, b.StartTime.Equal(FixedNow.Add(time.Hour)))
		assert.Equal(t, time.Hour, b.EndTime.Sub(b.StartTime))
	})

	t.Run("each builder gets a unique id", func(t *testing.T) {
		a := NewBookingBuilder(t).Build()
		c := NewBookingBuilder(t).Build()
		assert.NotEqual(t, a.ID, c.ID)
	})

	t.Run("With* override defaults and chain", func(t *testing.T) {
		room := Room(WithRoomID("room-9"))
		user := User(WithUserID("user-9"), WithRole(model.RoleManager))
		start := BaseStart
		end := BaseStart.Add(2 * time.Hour)

		b := NewBookingBuilder(t).
			WithRoom(room).
			WithUser(user).
			WithTime(start, end).
			WithStatus("cancelled").
			Build()

		assert.Equal(t, "room-9", b.RoomID)
		assert.Equal(t, "user-9", b.UserID)
		assert.True(t, b.StartTime.Equal(start))
		assert.True(t, b.EndTime.Equal(end))
		assert.Equal(t, model.StatusCancelled, b.Status)
	})
}
