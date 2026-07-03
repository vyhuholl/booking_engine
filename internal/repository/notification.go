package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Notification — доступ к БД для нотификатора: устойчивое хранилище обработанных
// событий (идемпотентность, таблица processed_events) и dead-letter недоставленных
// уведомлений (notification_dead_letter). Только чтение/запись, без бизнес-логики.
type Notification struct {
	pool *pgxpool.Pool
}

func NewNotification(pool *pgxpool.Pool) *Notification { return &Notification{pool: pool} }

// IsProcessed сообщает, обрабатывалось ли уже событие с данным eventID.
func (r *Notification) IsProcessed(ctx context.Context, eventID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM processed_events WHERE event_id = $1)`, eventID,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// MarkProcessed фиксирует eventID как обработанный. Идемпотентна: повторная
// фиксация того же eventID не ошибка (ON CONFLICT DO NOTHING).
func (r *Notification) MarkProcessed(ctx context.Context, eventID, eventType string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO processed_events (event_id, event_type) VALUES ($1, $2)
		 ON CONFLICT (event_id) DO NOTHING`,
		eventID, eventType,
	)
	return err
}

// SaveDeadLetter сохраняет недоставленное после ретраев уведомление.
func (r *Notification) SaveDeadLetter(ctx context.Context, eventID, userID, notificationType, errMsg string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO notification_dead_letter (event_id, user_id, notification_type, error)
		 VALUES ($1, $2, $3, $4)`,
		eventID, userID, notificationType, errMsg,
	)
	return err
}
