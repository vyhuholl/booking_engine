package testutil

import (
	"testing"

	"github.com/google/uuid"

	"github.com/example/booking-engine/internal/model"
)

// Object-mother хелперы: короткие имена для типовых сущностей в unit-тестах.
// В отличие от фикстур Room/User/Booking(opts...) не принимают опций и всегда
// выдают свежие уникальные ID — чтобы разные вызовы в одном тесте не
// коллизили. Внутри опираются на существующие билдеры (NewBookingBuilder,
// Room, User); для тонкой настройки бери их напрямую.

// TestRoom — обычная переговорка (8 мест, 2-й этаж), активная, свежий id.
func TestRoom(t *testing.T) model.Room {
	t.Helper()
	return Room(
		WithRoomID("room-"+uuid.NewString()),
		WithCapacity(8),
		WithFloor(2),
	)
}

// TestLargeRoom — большая переговорка (20 мест), активная, свежий id.
func TestLargeRoom(t *testing.T) model.Room {
	t.Helper()
	return Room(
		WithRoomID("room-"+uuid.NewString()),
		WithCapacity(20),
	)
}

// TestUser — пользователь с заданной ролью, свежими id и email.
func TestUser(t *testing.T, role string) model.User {
	t.Helper()
	id := "user-" + uuid.NewString()
	return User(
		WithUserID(id),
		WithEmail(id+"@example.com"),
		WithRole(model.Role(role)),
	)
}

// TestAdmin — сокращение для TestUser(t, "admin").
func TestAdmin(t *testing.T) model.User {
	t.Helper()
	return TestUser(t, string(model.RoleAdmin))
}

// TestManager — сокращение для TestUser(t, "manager").
func TestManager(t *testing.T) model.User {
	t.Helper()
	return TestUser(t, string(model.RoleManager))
}

// TestBooking — дефолтное бронирование: member в обычной комнате, окно через
// час от FixedNow на час (наследуется от NewBookingBuilder), свежий id брони.
func TestBooking(t *testing.T) model.Booking {
	t.Helper()
	return NewBookingBuilder(t).
		WithRoom(TestRoom(t)).
		WithUser(TestUser(t, string(model.RoleMember))).
		Build()
}

// TestConflictingBooking — бронь в той же комнате и в том же окне, что existing,
// поэтому гарантированно пересекается с ней при проверке занятости. Владелец и
// id брони — свежие (иной пользователь претендует на занятый слот).
func TestConflictingBooking(t *testing.T, existing model.Booking) model.Booking {
	t.Helper()
	// Та же комната по id + идентичное окно [start, end) → пересечение
	// гарантировано для любой валидной existing (start < end).
	room := Room(WithRoomID(existing.RoomID))
	return NewBookingBuilder(t).
		WithRoom(room).
		WithUser(TestUser(t, string(model.RoleMember))).
		WithTime(existing.StartTime, existing.EndTime).
		Build()
}
