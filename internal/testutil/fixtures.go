package testutil

import (
	"time"

	"github.com/example/booking-engine/internal/model"
)

// Детерминированные ID и временные якоря для unit-тестов сервисного слоя.
// Билдеры ниже используют их по умолчанию, поэтому тесты, сравнивающие поля
// на равенство, получают предсказуемые значения. Интеграционные сиды (seed.go)
// эти дефолты переопределяют на уникальные uuid.
const (
	RoomID      = "room-1"
	UserID      = "user-1"
	OtherUserID = "user-other"
	AdminID     = "admin-1"
	BookingID   = "b-1"
)

var (
	// FixedNow — момент «сейчас» для подмены service.now (см. Clock).
	FixedNow = time.Date(2026, 5, 19, 8, 0, 0, 0, time.UTC)
	// BaseStart/BaseEnd — типовое окно брони 10:00–11:00 того же дня.
	BaseStart = time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	BaseEnd   = BaseStart.Add(time.Hour)
	// MSK — не-UTC зона для проверок нормализации времени к UTC.
	MSK = time.FixedZone("MSK", 3*60*60)
)

// --- Room ----------------------------------------------------------------

// RoomOption переопределяет отдельное поле комнаты, собираемой Room.
type RoomOption func(*model.Room)

// Room собирает активную комнату с дефолтами (RoomID, cap 6, floor 2)
// и применяет опции по порядку.
func Room(opts ...RoomOption) model.Room {
	r := model.Room{
		ID:       RoomID,
		Name:     "Room 1",
		Capacity: 6,
		Floor:    2,
		Status:   model.RoomStatusActive,
	}
	for _, opt := range opts {
		opt(&r)
	}
	return r
}

func WithRoomID(id string) RoomOption {
	return func(r *model.Room) { r.ID = id }
}

func WithFloor(floor int) RoomOption {
	return func(r *model.Room) { r.Floor = floor }
}

func WithCapacity(n int) RoomOption {
	return func(r *model.Room) { r.Capacity = n }
}

func WithRoomStatus(s model.RoomStatus) RoomOption {
	return func(r *model.Room) { r.Status = s }
}

func WithEquipment(eq ...model.Equipment) RoomOption {
	return func(r *model.Room) { r.Equipment = eq }
}

// --- User ----------------------------------------------------------------

// UserOption переопределяет отдельное поле пользователя, собираемого User.
type UserOption func(*model.User)

// User собирает member'а с дефолтами (UserID) и применяет опции по порядку.
func User(opts ...UserOption) model.User {
	u := model.User{
		ID:    UserID,
		Name:  "Test User",
		Email: UserID + "@example.com",
		Role:  model.RoleMember,
	}
	for _, opt := range opts {
		opt(&u)
	}
	return u
}

func WithUserID(id string) UserOption {
	return func(u *model.User) { u.ID = id }
}

func WithRole(r model.Role) UserOption {
	return func(u *model.User) { u.Role = r }
}

// WithManagesFloor задаёт этаж, которым управляет менеджер (ManagesFloor).
func WithManagesFloor(floor int) UserOption {
	return func(u *model.User) { u.ManagesFloor = &floor }
}

func WithEmail(email string) UserOption {
	return func(u *model.User) { u.Email = email }
}

// --- Booking -------------------------------------------------------------

// BookingOption переопределяет отдельное поле брони, собираемой Booking.
type BookingOption func(*model.Booking)

// Booking собирает подтверждённую бронь на 1 час (BookingID, RoomID, UserID,
// окно BaseStart..+1h) и применяет опции по порядку.
func Booking(opts ...BookingOption) model.Booking {
	b := model.Booking{
		ID:        BookingID,
		RoomID:    RoomID,
		UserID:    UserID,
		Title:     "Standup",
		StartTime: BaseStart,
		EndTime:   BaseStart.Add(time.Hour),
		Status:    model.StatusConfirmed,
	}
	for _, opt := range opts {
		opt(&b)
	}
	return b
}

func WithBookingID(id string) BookingOption {
	return func(b *model.Booking) { b.ID = id }
}

func WithRoom(roomID string) BookingOption {
	return func(b *model.Booking) { b.RoomID = roomID }
}

func WithOwner(userID string) BookingOption {
	return func(b *model.Booking) { b.UserID = userID }
}

// WithStart сдвигает начало брони; конец выставляется на start+1h.
// Для иной длительности примените WithDuration после WithStart или WithInterval.
func WithStart(start time.Time) BookingOption {
	return func(b *model.Booking) {
		b.StartTime = start
		b.EndTime = start.Add(time.Hour)
	}
}

// WithInterval задаёт начало и конец явно.
func WithInterval(start, end time.Time) BookingOption {
	return func(b *model.Booking) {
		b.StartTime = start
		b.EndTime = end
	}
}

// WithDuration пересчитывает конец как start+d от текущего начала.
func WithDuration(d time.Duration) BookingOption {
	return func(b *model.Booking) { b.EndTime = b.StartTime.Add(d) }
}

func WithBookingStatus(s model.BookingStatus) BookingOption {
	return func(b *model.Booking) { b.Status = s }
}

// WithCreatedAt задаёт момент создания брони — якорь 24-часового таймаута одобрения
// (для интеграционных сидов; unit-фикстуры его не используют).
func WithCreatedAt(at time.Time) BookingOption {
	return func(b *model.Booking) { b.CreatedAt = at }
}

func WithTitle(title string) BookingOption {
	return func(b *model.Booking) { b.Title = title }
}
