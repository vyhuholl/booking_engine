package testutil

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
)

// SeedRoom вставляет комнату в БД и возвращает её id. По умолчанию id —
// свежий uuid (уникальность PK между строками); переопределяется WithRoomID.
func SeedRoom(t testing.TB, pool *pgxpool.Pool, opts ...RoomOption) string {
	t.Helper()
	id := "room-" + uuid.NewString()
	r := Room(append([]RoomOption{WithRoomID(id)}, opts...)...)

	equipment := make([]string, len(r.Equipment)) // не nil → пустой '{}', а не NULL
	for i, e := range r.Equipment {
		equipment[i] = string(e)
	}

	_, err := pool.Exec(context.Background(),
		`INSERT INTO rooms (id, name, capacity, floor, equipment, status)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		r.ID, r.Name, r.Capacity, r.Floor, equipment, r.Status)
	require.NoError(t, err)
	return r.ID
}

// SeedUser вставляет пользователя в БД и возвращает его id. По умолчанию id и
// email — свежие уникальные значения (email под UNIQUE-ограничением);
// переопределяются WithUserID / WithEmail.
func SeedUser(t testing.TB, pool *pgxpool.Pool, opts ...UserOption) string {
	t.Helper()
	id := "user-" + uuid.NewString()
	u := User(append([]UserOption{WithUserID(id), WithEmail(id + "@example.com")}, opts...)...)

	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, name, email, role, manages_floor)
		 VALUES ($1, $2, $3, $4, $5)`,
		u.ID, u.Name, u.Email, u.Role, u.ManagesFloor)
	require.NoError(t, err)
	return u.ID
}

// SeedBooking вставляет бронь напрямую (в обход repository.CreateChecked),
// чтобы аранжировать состояние без проверки пересечений.
func SeedBooking(t testing.TB, pool *pgxpool.Pool, b model.Booking) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO bookings (id, room_id, user_id, title, start_time, end_time, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		b.ID, b.RoomID, b.UserID, b.Title, b.StartTime, b.EndTime, b.Status)
	require.NoError(t, err)
}

// FreshRoomAndUser усекает данные (cleanup) и засевает одну комнату и одного
// пользователя — типовой пролог подтеста интеграционного слоя.
func FreshRoomAndUser(t testing.TB, pool *pgxpool.Pool, cleanup func()) (roomID, userID string) {
	t.Helper()
	cleanup()
	roomID = SeedRoom(t, pool)
	userID = SeedUser(t, pool)
	return roomID, userID
}
