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
