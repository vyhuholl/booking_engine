package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
)

// WaitlistID — дефолтный id waitlist-записи фикстуры (аналог BookingID).
const WaitlistID = "wl-1"

// WaitlistOption переопределяет отдельное поле записи, собираемой WaitlistEntry.
type WaitlistOption func(*model.WaitlistEntry)

// WaitlistEntry собирает waiting-запись листа ожидания (WaitlistID, RoomID, UserID,
// окно BaseStart..+1h, position 1) и применяет опции по порядку.
func WaitlistEntry(opts ...WaitlistOption) model.WaitlistEntry {
	e := model.WaitlistEntry{
		ID:        WaitlistID,
		RoomID:    RoomID,
		UserID:    UserID,
		StartTime: BaseStart,
		EndTime:   BaseStart.Add(time.Hour),
		Position:  1,
		Status:    model.WaitlistStatusWaiting,
		CreatedAt: FixedNow,
	}
	for _, opt := range opts {
		opt(&e)
	}
	return e
}

func WithWaitlistID(id string) WaitlistOption {
	return func(e *model.WaitlistEntry) { e.ID = id }
}

func WithWaitlistRoom(roomID string) WaitlistOption {
	return func(e *model.WaitlistEntry) { e.RoomID = roomID }
}

func WithWaitlistUser(userID string) WaitlistOption {
	return func(e *model.WaitlistEntry) { e.UserID = userID }
}

// WithWaitlistInterval задаёт интервал записи явно.
func WithWaitlistInterval(start, end time.Time) WaitlistOption {
	return func(e *model.WaitlistEntry) {
		e.StartTime = start
		e.EndTime = end
	}
}

func WithWaitlistPosition(pos int) WaitlistOption {
	return func(e *model.WaitlistEntry) { e.Position = pos }
}

func WithWaitlistStatus(s model.WaitlistStatus) WaitlistOption {
	return func(e *model.WaitlistEntry) { e.Status = s }
}

// WithOfferedAt переводит запись в статус offered с указанным моментом предложения.
func WithOfferedAt(at time.Time) WaitlistOption {
	return func(e *model.WaitlistEntry) {
		e.Status = model.WaitlistStatusOffered
		e.OfferedAt = &at
	}
}

// SeedWaitlist вставляет waitlist-запись напрямую (в обход repository.Create),
// чтобы аранжировать состояние очереди без автоназначения position.
func SeedWaitlist(t testing.TB, pool *pgxpool.Pool, e model.WaitlistEntry) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO waitlist_entries
		 (id, room_id, user_id, start_time, end_time, position, status, offered_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.ID, e.RoomID, e.UserID, e.StartTime, e.EndTime, e.Position, e.Status, e.OfferedAt, e.CreatedAt)
	require.NoError(t, err)
}
