package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/booking-engine/internal/model"
)

type Booking struct {
	pool *pgxpool.Pool
}

func NewBooking(pool *pgxpool.Pool) *Booking { return &Booking{pool: pool} }

func (r *Booking) Get(ctx context.Context, id string) (model.Booking, error) {
	var b model.Booking
	err := r.pool.QueryRow(ctx, `
        SELECT id, room_id, user_id, title, start_time, end_time, status
          FROM bookings WHERE id = $1`, id,
	).Scan(&b.ID, &b.RoomID, &b.UserID, &b.Title, &b.StartTime, &b.EndTime, &b.Status)
	if err != nil {
		return model.Booking{}, wrapNoRows(err)
	}
	return b, nil
}

// CreateChecked атомарно проверяет отсутствие пересечений и вставляет бронирование.
// Если найден конфликт — возвращает его (id/start/end) вместе с pgx.ErrNoRows-эквивалентом nil-ошибки вставки.
func (r *Booking) CreateChecked(ctx context.Context, b model.Booking) (conflict *model.Booking, err error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c model.Booking
	err = tx.QueryRow(ctx, `
        SELECT id, room_id, user_id, title, start_time, end_time, status
          FROM bookings
         WHERE room_id   = $1
           AND status    = 'confirmed'
           AND start_time < $3
           AND end_time   > $2
         LIMIT 1`,
		b.RoomID, b.StartTime, b.EndTime,
	).Scan(&c.ID, &c.RoomID, &c.UserID, &c.Title, &c.StartTime, &c.EndTime, &c.Status)
	switch {
	case err == nil:
		return &c, nil
	case err != pgx.ErrNoRows:
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
        INSERT INTO bookings (id, room_id, user_id, title, start_time, end_time, status)
        VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		b.ID, b.RoomID, b.UserID, b.Title, b.StartTime, b.EndTime, b.Status,
	); err != nil {
		return nil, err
	}
	return nil, tx.Commit(ctx)
}

func (r *Booking) Cancel(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE bookings SET status = 'cancelled' WHERE id = $1 AND status = 'confirmed'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type UserBookingFilter struct {
	UserID string
	Status *model.BookingStatus
	From   *time.Time
	To     *time.Time
}

func (r *Booking) ListByUser(ctx context.Context, f UserBookingFilter) ([]model.Booking, error) {
	args := []any{f.UserID}
	where := []string{"user_id = $1"}
	if f.Status != nil {
		args = append(args, *f.Status)
		where = append(where, "status = $"+itoa(len(args)))
	}
	if f.From != nil {
		args = append(args, *f.From)
		where = append(where, "start_time >= $"+itoa(len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		where = append(where, "start_time <= $"+itoa(len(args)))
	}
	q := "SELECT id, room_id, user_id, title, start_time, end_time, status FROM bookings WHERE " +
		join(where, " AND ") + " ORDER BY start_time"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookings(rows)
}

// ListByRoomOnDate возвращает бронирования комнаты, пересекающие сутки [date, date+24h) в UTC.
func (r *Booking) ListByRoomOnDate(ctx context.Context, roomID string, date time.Time) ([]model.Booking, error) {
	dayStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	rows, err := r.pool.Query(ctx, `
        SELECT id, room_id, user_id, title, start_time, end_time, status
          FROM bookings
         WHERE room_id = $1
           AND start_time < $3
           AND end_time   > $2
         ORDER BY start_time`,
		roomID, dayStart, dayEnd,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookings(rows)
}

// CountByRoomInPeriod возвращает число бронирований комнаты, чей start_time
// попадает в полуоткрытый интервал [from, to). Учитываются брони в любом
// статусе (как и ListByRoomOnDate — это картина использования комнаты, а не
// только подтверждённых броней).
func (r *Booking) CountByRoomInPeriod(ctx context.Context, roomID string, from, to time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
        SELECT COUNT(*) FROM bookings
         WHERE room_id    = $1
           AND start_time >= $2
           AND start_time <  $3`,
		roomID, from, to,
	).Scan(&n)
	return n, err
}

func (r *Booking) HasActiveForRoom(ctx context.Context, roomID string, after time.Time) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM bookings
             WHERE room_id = $1 AND status = 'confirmed' AND end_time > $2
        )`, roomID, after,
	).Scan(&exists)
	return exists, err
}

func scanBookings(rows pgx.Rows) ([]model.Booking, error) {
	out := make([]model.Booking, 0)
	for rows.Next() {
		var b model.Booking
		if err := rows.Scan(&b.ID, &b.RoomID, &b.UserID, &b.Title, &b.StartTime, &b.EndTime, &b.Status); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
