package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/booking-engine/internal/model"
)

type User struct {
	pool *pgxpool.Pool
}

func NewUser(pool *pgxpool.Pool) *User { return &User{pool: pool} }

func (r *User) Get(ctx context.Context, id string) (model.User, error) {
	var u model.User
	err := r.pool.QueryRow(ctx,
		`SELECT id, name, email, role, manages_floor FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.ManagesFloor)
	if err != nil {
		return model.User{}, wrapNoRows(err)
	}
	return u, nil
}

// ListAdmins возвращает всех пользователей с ролью admin. Используется
// нотификатором для рассылки события booking.pending_approval всем админам.
// Пустой список — не ошибка (админов может не быть).
func (r *User) ListAdmins(ctx context.Context) ([]model.User, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, name, email, role, manages_floor FROM users WHERE role = 'admin' ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var admins []model.User
	for rows.Next() {
		var u model.User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.ManagesFloor); err != nil {
			return nil, err
		}
		admins = append(admins, u)
	}
	return admins, rows.Err()
}
