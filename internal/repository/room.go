package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/booking-engine/internal/model"
)

type Room struct {
	pool *pgxpool.Pool
}

func NewRoom(pool *pgxpool.Pool) *Room { return &Room{pool: pool} }

type RoomFilter struct {
	Floor  *int
	Limit  int
	Offset int
}

func (r *Room) List(ctx context.Context, f RoomFilter) ([]model.Room, int, error) {
	args := []any{}
	where := ""
	if f.Floor != nil {
		args = append(args, *f.Floor)
		where = "WHERE floor = $1"
	}

	var total int
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM rooms "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit, f.Offset)
	query := "SELECT id, name, capacity, floor, equipment FROM rooms " + where +
		" ORDER BY floor, name LIMIT $" + itoa(len(args)-1) + " OFFSET $" + itoa(len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	return scanRooms(rows, total)
}

func (r *Room) Get(ctx context.Context, id string) (model.Room, error) {
	var room model.Room
	var equipment []string
	err := r.pool.QueryRow(ctx,
		`SELECT id, name, capacity, floor, equipment FROM rooms WHERE id = $1`, id,
	).Scan(&room.ID, &room.Name, &room.Capacity, &room.Floor, &equipment)
	if err != nil {
		return model.Room{}, wrapNoRows(err)
	}
	room.Equipment = toEquipment(equipment)
	return room, nil
}

func (r *Room) Create(ctx context.Context, room model.Room) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO rooms (id, name, capacity, floor, equipment) VALUES ($1, $2, $3, $4, $5)`,
		room.ID, room.Name, room.Capacity, room.Floor, equipmentToStrings(room.Equipment),
	)
	return err
}

func (r *Room) Update(ctx context.Context, room model.Room) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE rooms SET name = $2, capacity = $3, floor = $4, equipment = $5 WHERE id = $1`,
		room.ID, room.Name, room.Capacity, room.Floor, equipmentToStrings(room.Equipment),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Room) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM rooms WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Available возвращает комнаты, у которых нет confirmed-бронирований,
// пересекающихся с интервалом [start, end), и которые удовлетворяют фильтрам.
func (r *Room) Available(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error) {
	args := []any{start, end}
	where := []string{
		`NOT EXISTS (
            SELECT 1 FROM bookings b
            WHERE b.room_id = r.id
              AND b.status = 'confirmed'
              AND b.start_time < $2
              AND b.end_time   > $1
        )`,
	}
	if capacityMin != nil {
		args = append(args, *capacityMin)
		where = append(where, "r.capacity >= $"+itoa(len(args)))
	}
	if floor != nil {
		args = append(args, *floor)
		where = append(where, "r.floor = $"+itoa(len(args)))
	}
	if len(equipment) > 0 {
		args = append(args, equipment)
		where = append(where, "r.equipment @> $"+itoa(len(args)))
	}

	q := "SELECT id, name, capacity, floor, equipment FROM rooms r WHERE " + join(where, " AND ") + " ORDER BY r.floor, r.name"
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out, _, err := scanRooms(rows, 0)
	return out, err
}

func scanRooms(rows pgx.Rows, total int) ([]model.Room, int, error) {
	out := make([]model.Room, 0)
	for rows.Next() {
		var room model.Room
		var equipment []string
		if err := rows.Scan(&room.ID, &room.Name, &room.Capacity, &room.Floor, &equipment); err != nil {
			return nil, 0, err
		}
		room.Equipment = toEquipment(equipment)
		out = append(out, room)
	}
	return out, total, rows.Err()
}

func toEquipment(s []string) []model.Equipment {
	out := make([]model.Equipment, 0, len(s))
	for _, v := range s {
		out = append(out, model.Equipment(v))
	}
	return out
}

func equipmentToStrings(e []model.Equipment) []string {
	out := make([]string, 0, len(e))
	for _, v := range e {
		out = append(out, string(v))
	}
	return out
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	bp := len(b)
	for i > 0 {
		bp--
		b[bp] = digits[i%10]
		i /= 10
	}
	if neg {
		bp--
		b[bp] = '-'
	}
	return string(b[bp:])
}

func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(sep) * (len(parts) - 1)
	for _, p := range parts {
		n += len(p)
	}
	buf := make([]byte, 0, n)
	buf = append(buf, parts[0]...)
	for _, p := range parts[1:] {
		buf = append(buf, sep...)
		buf = append(buf, p...)
	}
	return string(buf)
}
