package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/booking-engine/internal/model"
)

type Booking struct {
	pool *pgxpool.Pool
}

func NewBooking(pool *pgxpool.Pool) *Booking { return &Booking{pool: pool} }

// bookingColumns — общий список колонок брони в порядке scanBooking. Единый список,
// чтобы SELECT'ы и RETURNING не расходились со сканером.
const bookingColumns = "id, room_id, user_id, title, start_time, end_time, status, rejection_reason, created_at"

// activeStatuses — список статусов, при которых бронь удерживает слот комнаты
// и учитывается в проверке пересечений/занятости. Собран из
// model.ActiveBookingStatuses — единого источника правды, чтобы предикаты занятости
// в разных запросах не разошлись. Используется в SQL как параметр для ANY($N).
var activeStatuses = buildStatusList(model.ActiveBookingStatuses)

// buildStatusList преобразует статусы модели в срез строк для использования в
// SQL-запросах с ANY($N) (параметризованный запрос, без интерполяции).
func buildStatusList(ss []model.BookingStatus) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// scanBooking сканирует одну строку брони (все колонки в порядке bookingColumns).
func scanBooking(row pgx.Row) (model.Booking, error) {
	var b model.Booking
	err := row.Scan(&b.ID, &b.RoomID, &b.UserID, &b.Title, &b.StartTime, &b.EndTime,
		&b.Status, &b.RejectionReason, &b.CreatedAt)
	return b, err
}

func (r *Booking) Get(ctx context.Context, id string) (model.Booking, error) {
	b, err := scanBooking(r.pool.QueryRow(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE id = $1`, id))
	if err != nil {
		return model.Booking{}, wrapNoRows(err)
	}
	return b, nil
}

// CreateChecked атомарно проверяет отсутствие пересечений и вставляет бронирование.
// Слот считается занятым любой активной бронью (activeStatuses: confirmed,
// pending_approval, approved) — pending_approval/approved резервируют интервал так же,
// как confirmed. Если найден конфликт — возвращает его (*model.Booking, nil), иначе
// (nil, nil).
func (r *Booking) CreateChecked(ctx context.Context, b model.Booking) (conflict *model.Booking, err error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	c, scanErr := scanBooking(tx.QueryRow(ctx, `
	SELECT `+bookingColumns+`
	FROM bookings
	WHERE room_id   = $1
	AND status    = ANY($4)
	AND start_time < $3
	AND end_time   > $2
	LIMIT 1`,
	b.RoomID, b.StartTime, b.EndTime, activeStatuses,
	))
	switch {
	case scanErr == nil:
		return &c, nil
	case !errors.Is(scanErr, pgx.ErrNoRows):
		return nil, scanErr
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
		`UPDATE bookings SET status = 'cancelled' WHERE id = $1 AND status = ANY($2)`, id, activeStatuses)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CancelAndOfferWaitlist атомарно (Serializable) отменяет бронь и предлагает
// освободившийся слот первой (по position) waiting-записи листа ожидания на
// пересекающийся интервал. Отменить можно любую активную бронь (activeStatuses —
// в т.ч. pending_approval/approved: отмена владельцем имеет приоритет над одобрением).
// Отмена и предложение — в одной транзакции: слот не может быть перехвачен между
// отменой и предложением. Возвращает отменённую бронь и опционально предложенную
// waitlist-запись (nil, если очереди на слот нет). 0 отменённых строк → ErrNotFound
// (бронь не существует или уже неактивна — гонка).
func (r *Booking) CancelAndOfferWaitlist(ctx context.Context, id string, now time.Time) (model.Booking, *model.WaitlistEntry, error) {
	var b model.Booking
	var offered *model.WaitlistEntry
	err := runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		b = model.Booking{} // сброс перед каждой попыткой
		offered = nil

		var scanErr error
		b, scanErr = scanBooking(tx.QueryRow(ctx, `
		UPDATE bookings SET status = 'cancelled'
		WHERE id = $1 AND status = ANY($2)
		RETURNING `+bookingColumns+``, id, activeStatuses))
		switch {
		case errors.Is(scanErr, pgx.ErrNoRows):
			return ErrNotFound
		case scanErr != nil:
			return scanErr
		}

		o, offerErr := offerNextSlot(ctx, tx, b.RoomID, b.StartTime, b.EndTime, now)
		if offerErr != nil {
			return offerErr
		}
		offered = o
		return nil
	})
	if err != nil {
		return model.Booking{}, nil, err
	}
	return b, offered, nil
}

// Approve атомарно (Serializable) переводит бронь из pending_approval в approved.
// Условный UPDATE (id + status='pending_approval') — точка идемпотентности и
// сериализации: из двух одновременных approve ровно один увидит pending и обновит
// строку, второй получит 0 строк → ErrNotFound (бронь уже не pending). Таймаут здесь
// НЕ проверяется — это делает сервис по Booking.CreatedAt (параметр now принят для
// симметрии сигнатуры с Reject; методом не используется).
func (r *Booking) Approve(ctx context.Context, id string, now time.Time) (model.Booking, error) {
	_ = now
	var b model.Booking
	err := runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		var scanErr error
		b, scanErr = scanBooking(tx.QueryRow(ctx, `
            UPDATE bookings SET status = 'approved'
             WHERE id = $1 AND status = 'pending_approval'
            RETURNING `+bookingColumns, id))
		switch {
		case errors.Is(scanErr, pgx.ErrNoRows):
			return ErrNotFound
		case scanErr != nil:
			return scanErr
		}
		return nil
	})
	if err != nil {
		return model.Booking{}, err
	}
	return b, nil
}

// RejectAndOfferWaitlist атомарно (Serializable) отклоняет бронь на согласовании и
// предлагает освободившийся слот листу ожидания — как CancelAndOfferWaitlist для
// отмены. Условный UPDATE (id + status='pending_approval') даёт идемпотентность и
// безопасность гонки: 0 строк → ErrNotFound. Возвращает отклонённую бронь (со
// статусом rejected и сохранённой причиной) и опционально предложенную waitlist-запись.
func (r *Booking) RejectAndOfferWaitlist(ctx context.Context, id, reason string, now time.Time) (model.Booking, *model.WaitlistEntry, error) {
	var b model.Booking
	var offered *model.WaitlistEntry
	err := runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		b = model.Booking{} // сброс перед каждой попыткой
		offered = nil
		var rErr error
		b, offered, rErr = rejectPending(ctx, tx, id, reason, now)
		return rErr
	})
	if err != nil {
		return model.Booking{}, nil, err
	}
	return b, offered, nil
}

// rejectPending переводит одну pending_approval-бронь в rejected с причиной и в той
// же транзакции предлагает освободившийся слот листу ожидания (offerNextSlot).
// Условный UPDATE (id + status='pending_approval') — 0 строк → ErrNotFound. Общий для
// RejectAndOfferWaitlist (по id) и подметания просроченных в ListPendingApprovals.
func rejectPending(ctx context.Context, tx pgx.Tx, id, reason string, now time.Time) (model.Booking, *model.WaitlistEntry, error) {
	b, scanErr := scanBooking(tx.QueryRow(ctx, `
        UPDATE bookings SET status = 'rejected', rejection_reason = $2
         WHERE id = $1 AND status = 'pending_approval'
        RETURNING `+bookingColumns, id, reason))
	switch {
	case errors.Is(scanErr, pgx.ErrNoRows):
		return model.Booking{}, nil, ErrNotFound
	case scanErr != nil:
		return model.Booking{}, nil, scanErr
	}
	offered, offerErr := offerNextSlot(ctx, tx, b.RoomID, b.StartTime, b.EndTime, now)
	if offerErr != nil {
		return model.Booking{}, nil, offerErr
	}
	return b, offered, nil
}

// ListPendingApprovals возвращает брони, ожидающие одобрения, отсортированные по
// времени создания. Перед выборкой в той же транзакции (Serializable) «подметает»
// просроченные: бронь в pending_approval старше timeout от now авто-отклоняется
// (rejected + reason), её слот освобождается и предлагается листу ожидания — как при
// обычном reject. Возвращает (оставшиеся pending, авто-отклонённые); авто-отклонённые
// нужны сервису для публикации событий booking.rejected.
func (r *Booking) ListPendingApprovals(ctx context.Context, now time.Time, timeout time.Duration, reason string) (pending []model.Booking, autoRejected []model.Booking, err error) {
	cutoff := now.Add(-timeout)
	err = runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		pending = nil // сброс перед каждой попыткой
		autoRejected = nil

		// 1. Собрать id просроченных. Набор строк полностью читаем до апдейтов: на
		//    одном соединении нельзя выполнять запросы при открытом наборе строк.
		rows, qErr := tx.Query(ctx, `
            SELECT id FROM bookings
             WHERE status = 'pending_approval' AND created_at < $1
             ORDER BY created_at
             FOR UPDATE`, cutoff)
		if qErr != nil {
			return qErr
		}
		var expired []string
		for rows.Next() {
			var id string
			if scanErr := rows.Scan(&id); scanErr != nil {
				rows.Close()
				return scanErr
			}
			expired = append(expired, id)
		}
		rows.Close()
		if rowsErr := rows.Err(); rowsErr != nil {
			return rowsErr
		}

		// 2. Авто-reject каждого просроченного + предложение слота листу ожидания.
		for _, id := range expired {
			b, _, rErr := rejectPending(ctx, tx, id, reason, now)
			if rErr != nil {
				return rErr
			}
			autoRejected = append(autoRejected, b)
		}

		// 3. Оставшиеся брони, ожидающие одобрения.
		remaining, lErr := tx.Query(ctx, `
            SELECT `+bookingColumns+` FROM bookings
             WHERE status = 'pending_approval'
             ORDER BY created_at`)
		if lErr != nil {
			return lErr
		}
		defer remaining.Close()
		pending, lErr = scanBookings(remaining)
		return lErr
	})
	if err != nil {
		return nil, nil, err
	}
	return pending, autoRejected, nil
}

// ListConflicting возвращает активные (activeStatuses) брони комнаты, пересекающие
// интервал [start, end), отсортированные по времени начала. Тот же предикат
// пересечения, что у IsRoomBusy/CreateChecked (полуоткрытые интервалы: касание
// границей не конфликт). Пустой результат — не nil.
func (r *Booking) ListConflicting(ctx context.Context, roomID string, start, end time.Time) ([]model.Booking, error) {
	rows, err := r.pool.Query(ctx, `
	SELECT `+bookingColumns+`
	FROM bookings
	WHERE room_id    = $1
	AND status     = ANY($4)
	AND start_time < $3
	AND end_time   > $2
	ORDER BY start_time`,
	roomID, start, end, activeStatuses,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookings(rows)
}

// IsRoomBusy сообщает, есть ли активная бронь комнаты (activeStatuses: confirmed,
// pending_approval, approved), пересекающая интервал [start, end). Используется листом
// ожидания: вставать в очередь можно только на занятый интервал — теперь занятость
// создаёт и бронь на согласовании/одобренная, а не только подтверждённая.
func (r *Booking) IsRoomBusy(ctx context.Context, roomID string, start, end time.Time) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
	SELECT EXISTS (
	SELECT 1 FROM bookings
	WHERE room_id = $1 AND status = ANY($4)
	AND start_time < $3 AND end_time > $2
	)`, roomID, start, end, activeStatuses,
	).Scan(&exists)
	return exists, err
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
	q := "SELECT " + bookingColumns + " FROM bookings WHERE " +
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
        SELECT `+bookingColumns+`
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

// ListByRoomInPeriod возвращает бронирования комнаты, чей start_time попадает
// в полуоткрытый интервал [from, to), отсортированные по времени начала.
// Как и ListByRoomOnDate/CountByRoomInPeriod, учитываются брони в любом статусе —
// это картина использования комнаты, а не только активных броней.
func (r *Booking) ListByRoomInPeriod(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT `+bookingColumns+`
          FROM bookings
         WHERE room_id    = $1
           AND start_time >= $2
           AND start_time <  $3
         ORDER BY start_time`,
		roomID, from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBookings(rows)
}

// GetBookingsByDateRange возвращает бронирования комнаты со start_time в
// полуоткрытом интервале [from, to). Выборка идентична ListByRoomInPeriod (брони
// в любом статусе, сортировка по времени начала; фильтрация по статусу/пользователю
// выполняется в сервисе) — это её доменно именованный псевдоним для недельного отчёта.
func (r *Booking) GetBookingsByDateRange(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error) {
	return r.ListByRoomInPeriod(ctx, roomID, from, to)
}

// CountByRoomInPeriod возвращает число бронирований комнаты, чей start_time
// попадает в полуоткрытый интервал [from, to). Учитываются брони в любом
// статусе (как и ListByRoomOnDate — это картина использования комнаты, а не
// только активных броней).
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

// HasActiveForRoom сообщает, есть ли у комнаты активная (activeStatuses) будущая
// бронь — используется как страховка перед удалением комнаты. Бронь на согласовании
// (pending_approval) и одобренная (approved) тоже удерживают комнату, как confirmed.
func (r *Booking) HasActiveForRoom(ctx context.Context, roomID string, after time.Time) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
	SELECT EXISTS (
	SELECT 1 FROM bookings
	WHERE room_id = $1 AND status = ANY($3) AND end_time > $2
	)`, roomID, after, activeStatuses,
	).Scan(&exists)
	return exists, err
}

func scanBookings(rows pgx.Rows) ([]model.Booking, error) {
	out := make([]model.Booking, 0)
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
