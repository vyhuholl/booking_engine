package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/booking-engine/internal/model"
)

type Waitlist struct {
	pool *pgxpool.Pool
}

func NewWaitlist(pool *pgxpool.Pool) *Waitlist { return &Waitlist{pool: pool} }

// waitlistColumns — общий список колонок в порядке scanWaitlistEntry.
const waitlistColumns = "id, room_id, user_id, start_time, end_time, position, status, offered_at, created_at"

func scanWaitlistEntry(row pgx.Row) (model.WaitlistEntry, error) {
	var e model.WaitlistEntry
	err := row.Scan(&e.ID, &e.RoomID, &e.UserID, &e.StartTime, &e.EndTime,
		&e.Position, &e.Status, &e.OfferedAt, &e.CreatedAt)
	return e, err
}

// Create в одной транзакции Serializable (с ретраями при сериализационных сбоях):
//  1. проверяет, что комната ЗАНЯТА подтверждённой бронью на интервал — иначе
//     ErrNoOverlap (запись в очередь не нужна); проверка атомарна с вставкой, поэтому
//     параллельная отмена брони не создаёт запись на уже свободный слот;
//  2. вычисляет position (следующий номер среди активных записей комнаты);
//  3. вставляет запись. Нарушение uq_waitlist_active нормализуется в ErrConflict.
func (r *Waitlist) Create(ctx context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error) {
	err := runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		var busy bool
		if err := tx.QueryRow(ctx, `
            SELECT EXISTS (
                SELECT 1 FROM bookings
                 WHERE room_id = $1 AND status = 'confirmed'
                   AND start_time < $3 AND end_time > $2
            )`, e.RoomID, e.StartTime, e.EndTime,
		).Scan(&busy); err != nil {
			return err
		}
		if !busy {
			return ErrNoOverlap
		}

		var position int
		if err := tx.QueryRow(ctx, `
            SELECT COALESCE(MAX(position), 0) + 1
              FROM waitlist_entries
             WHERE room_id = $1 AND status IN ('waiting', 'offered')`,
			e.RoomID,
		).Scan(&position); err != nil {
			return err
		}
		e.Position = position

		if _, err := tx.Exec(ctx, `
            INSERT INTO waitlist_entries (`+waitlistColumns+`)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			e.ID, e.RoomID, e.UserID, e.StartTime, e.EndTime, e.Position, e.Status, e.OfferedAt, e.CreatedAt,
		); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		return nil
	})
	if err != nil {
		return model.WaitlistEntry{}, err
	}
	return e, nil
}

func (r *Waitlist) Get(ctx context.Context, id string) (model.WaitlistEntry, error) {
	e, err := scanWaitlistEntry(r.pool.QueryRow(ctx,
		`SELECT `+waitlistColumns+` FROM waitlist_entries WHERE id = $1`, id))
	if err != nil {
		return model.WaitlistEntry{}, wrapNoRows(err)
	}
	return e, nil
}

// ListByRoom возвращает очередь комнаты, отсортированную по position (пустой срез, не nil).
func (r *Waitlist) ListByRoom(ctx context.Context, roomID string) ([]model.WaitlistEntry, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+waitlistColumns+` FROM waitlist_entries WHERE room_id = $1 ORDER BY position`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.WaitlistEntry, 0)
	for rows.Next() {
		e, err := scanWaitlistEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Waitlist) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM waitlist_entries WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAndOfferNext удаляет запись и, если она была offered, в той же транзакции
// (Serializable, с ретраями) предлагает освободившийся слот следующей подходящей
// waiting-записи. Так выход владельца из offered-слота не «подвешивает» очередь:
// слот честно уходит следующему, как при протухании. Возвращает предложенную запись
// или nil (для не-offered записей и когда кандидатов нет). 0 удалённых строк →
// ErrNotFound.
func (r *Waitlist) DeleteAndOfferNext(ctx context.Context, id string, now time.Time) (offered *model.WaitlistEntry, err error) {
	err = runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		offered = nil // сброс перед каждой попыткой

		var status model.WaitlistStatus
		var roomID string
		var start, end time.Time
		scanErr := tx.QueryRow(ctx, `
            DELETE FROM waitlist_entries WHERE id = $1
            RETURNING status, room_id, start_time, end_time`, id,
		).Scan(&status, &roomID, &start, &end)
		switch {
		case errors.Is(scanErr, pgx.ErrNoRows):
			return ErrNotFound
		case scanErr != nil:
			return scanErr
		}

		if status != model.WaitlistStatusOffered {
			return nil // ждавшая/протухшая/сконвертированная запись слот не держала
		}
		o, offerErr := offerNextSlot(ctx, tx, roomID, start, end, now)
		if offerErr != nil {
			return offerErr
		}
		offered = o
		return nil
	})
	if err != nil {
		return nil, err
	}
	return offered, nil
}

// ConfirmAndBook атомарно (Serializable, с ретраями при сериализационных сбоях)
// гасит offered-запись и создаёт бронь. Условный UPDATE (id + status='offered') —
// точка сериализации: из двух одновременных confirm ровно один увидит статус offered
// и продолжит, второй получит ErrNotFound. Если интервал уже занят — возвращает
// конфликтующую бронь (транзакция откатывается, запись остаётся offered).
func (r *Waitlist) ConfirmAndBook(ctx context.Context, entryID string, b model.Booking) (conflict *model.Booking, err error) {
	err = runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		conflict = nil // сброс перед каждой попыткой

		tag, execErr := tx.Exec(ctx,
			`UPDATE waitlist_entries SET status = 'converted' WHERE id = $1 AND status = 'offered'`, entryID)
		if execErr != nil {
			return execErr
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound // гонка: запись уже не offered
		}

		var c model.Booking
		scanErr := tx.QueryRow(ctx, `
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
		case scanErr == nil:
			conflict = &c
			return errRollback // откатить конвертацию: слот занят, запись остаётся offered
		case scanErr != pgx.ErrNoRows:
			return scanErr
		}

		if _, execErr := tx.Exec(ctx, `
            INSERT INTO bookings (id, room_id, user_id, title, start_time, end_time, status)
            VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			b.ID, b.RoomID, b.UserID, b.Title, b.StartTime, b.EndTime, b.Status,
		); execErr != nil {
			return execErr
		}
		return nil
	})
	switch {
	case errors.Is(err, errRollback):
		return conflict, nil
	case err != nil:
		return nil, err
	}
	return nil, nil
}

// ExpireAndOfferNext атомарно (Serializable, с ретраями) переводит протухшую
// offered-запись в expired и предлагает слот следующей подходящей waiting-записи
// (по position). Возвращает предложенную запись или nil, если кандидатов нет.
func (r *Waitlist) ExpireAndOfferNext(ctx context.Context, entryID string, now time.Time) (offered *model.WaitlistEntry, err error) {
	err = runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		offered = nil // сброс перед каждой попыткой

		var roomID string
		var start, end time.Time
		scanErr := tx.QueryRow(ctx, `
            UPDATE waitlist_entries SET status = 'expired'
             WHERE id = $1 AND status = 'offered'
            RETURNING room_id, start_time, end_time`, entryID,
		).Scan(&roomID, &start, &end)
		switch {
		case errors.Is(scanErr, pgx.ErrNoRows):
			return ErrNotFound // гонка: запись уже не offered
		case scanErr != nil:
			return scanErr
		}

		o, offerErr := offerNextSlot(ctx, tx, roomID, start, end, now)
		if offerErr != nil {
			return offerErr
		}
		offered = o
		return nil
	})
	if err != nil {
		return nil, err
	}
	return offered, nil
}

// offerNextSlot находит первую (по position) waiting-запись комнаты, чей интервал
// пересекается со слотом [start, end), и переводит её в offered. FOR UPDATE SKIP
// LOCKED гарантирует, что параллельные операции не предложат одну запись дважды.
// Возвращает предложенную запись или nil, если подходящих waiting-записей нет.
func offerNextSlot(ctx context.Context, tx pgx.Tx, roomID string, start, end, now time.Time) (*model.WaitlistEntry, error) {
	e, err := scanWaitlistEntry(tx.QueryRow(ctx, `
        WITH next AS (
            SELECT id FROM waitlist_entries
             WHERE room_id = $1 AND status = 'waiting'
               AND start_time < $3 AND end_time > $2
             ORDER BY position
             LIMIT 1
             FOR UPDATE SKIP LOCKED
        )
        UPDATE waitlist_entries w
           SET status = 'offered', offered_at = $4
          FROM next
         WHERE w.id = next.id
        RETURNING w.`+"id, w.room_id, w.user_id, w.start_time, w.end_time, w.position, w.status, w.offered_at, w.created_at",
		roomID, start, end, now,
	))
	switch {
	case err == nil:
		return &e, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil // очереди на слот нет — не ошибка
	default:
		return nil, err
	}
}
