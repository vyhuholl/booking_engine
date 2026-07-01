package testutil

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/example/booking-engine/internal/model"
)

// BookingBuilder — цепочечный (fluent) конструктор model.Booking для
// unit-тестов. В отличие от Booking(opts...) сам подставляет дефолтные комнату
// и пользователя, если WithRoom/WithUser не заданы, и выдаёт брони свежий uuid.
// Терминальный метод — Build.
//
// Комната и пользователь хранятся указателями и остаются nil до Build: там
// подставляются дефолтные Room() и member-User(), если не были заданы явно.
type BookingBuilder struct {
	t      *testing.T
	id     string
	room   *model.Room
	user   *model.User
	start  time.Time
	end    time.Time
	status model.BookingStatus
}

// NewBookingBuilder создаёт билдер с дефолтами: свежий uuid брони, окно в час
// (начало через час от FixedNow, длительность час) и статус confirmed.
// Комната и пользователь подставляются лениво в Build.
func NewBookingBuilder(t *testing.T) *BookingBuilder {
	t.Helper()
	start := FixedNow.Add(time.Hour)
	return &BookingBuilder{
		t:      t,
		id:     "b-" + uuid.NewString(),
		start:  start,
		end:    start.Add(time.Hour),
		status: model.StatusConfirmed,
	}
}

// WithRoom задаёт комнату брони (RoomID берётся из неё в Build).
func (b *BookingBuilder) WithRoom(room model.Room) *BookingBuilder {
	b.room = &room
	return b
}

// WithUser задаёт владельца брони (UserID берётся из него в Build).
func (b *BookingBuilder) WithUser(user model.User) *BookingBuilder {
	b.user = &user
	return b
}

// WithTime задаёт начало и конец брони явно.
func (b *BookingBuilder) WithTime(start, end time.Time) *BookingBuilder {
	b.start = start
	b.end = end
	return b
}

// WithStatus задаёт статус брони строкой (напр. "confirmed", "cancelled").
func (b *BookingBuilder) WithStatus(status string) *BookingBuilder {
	b.status = model.BookingStatus(status)
	return b
}

// Build собирает model.Booking, подставляя дефолтные комнату и пользователя,
// если они не были заданы через WithRoom/WithUser.
func (b *BookingBuilder) Build() model.Booking {
	b.t.Helper()

	room := b.room
	if room == nil {
		r := Room()
		room = &r
	}

	user := b.user
	if user == nil {
		u := User(WithRole(model.RoleMember))
		user = &u
	}

	return model.Booking{
		ID:        b.id,
		RoomID:    room.ID,
		UserID:    user.ID,
		Title:     "Standup",
		StartTime: b.start,
		EndTime:   b.end,
		Status:    b.status,
	}
}
