package testutil

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/model"
)

func TestTestRoom(t *testing.T) {
	r := TestRoom(t)
	assert.True(t, strings.HasPrefix(r.ID, "room-"), "id should carry room- prefix, got %q", r.ID)
	assert.Equal(t, 8, r.Capacity)
	assert.Equal(t, 2, r.Floor)
	assert.Equal(t, model.RoomStatusActive, r.Status)

	assert.NotEqual(t, TestRoom(t).ID, TestRoom(t).ID, "each call gets a unique id")
}

func TestTestLargeRoom(t *testing.T) {
	r := TestLargeRoom(t)
	assert.True(t, strings.HasPrefix(r.ID, "room-"), "id should carry room- prefix, got %q", r.ID)
	assert.Equal(t, 20, r.Capacity)
	assert.Equal(t, model.RoomStatusActive, r.Status)

	assert.NotEqual(t, TestLargeRoom(t).ID, TestLargeRoom(t).ID, "each call gets a unique id")
}

func TestTestUser(t *testing.T) {
	t.Run("role and unique id/email", func(t *testing.T) {
		u := TestUser(t, "member")
		assert.True(t, strings.HasPrefix(u.ID, "user-"), "id should carry user- prefix, got %q", u.ID)
		assert.Equal(t, model.RoleMember, u.Role)
		assert.Equal(t, u.ID+"@example.com", u.Email)

		assert.NotEqual(t, TestUser(t, "member").ID, TestUser(t, "member").ID, "each call gets a unique id")
	})

	t.Run("shortcuts carry their role", func(t *testing.T) {
		assert.Equal(t, model.RoleAdmin, TestAdmin(t).Role)
		assert.Equal(t, model.RoleManager, TestManager(t).Role)
	})
}

func TestTestBooking(t *testing.T) {
	b := TestBooking(t)
	assert.True(t, strings.HasPrefix(b.ID, "b-"), "id should carry b- prefix, got %q", b.ID)
	assert.True(t, strings.HasPrefix(b.RoomID, "room-"), "room id should be unique, got %q", b.RoomID)
	assert.True(t, strings.HasPrefix(b.UserID, "user-"), "user id should be unique, got %q", b.UserID)
	assert.Equal(t, model.StatusConfirmed, b.Status)

	assert.NotEqual(t, TestBooking(t).ID, TestBooking(t).ID, "each call gets a unique id")
}

func TestTestConflictingBooking(t *testing.T) {
	existing := TestBooking(t)
	conflict := TestConflictingBooking(t, existing)

	// Пересечение по правилу [start, end): newStart < existEnd && newEnd > existStart.
	assert.True(t, conflict.StartTime.Before(existing.EndTime), "should start before existing ends")
	assert.True(t, conflict.EndTime.After(existing.StartTime), "should end after existing starts")

	assert.Equal(t, existing.RoomID, conflict.RoomID, "must contend for the same room")
	assert.NotEqual(t, existing.ID, conflict.ID, "must be a distinct booking")
	assert.NotEqual(t, existing.UserID, conflict.UserID, "should belong to another user")
}
