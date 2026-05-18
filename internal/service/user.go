package service

import (
	"context"
	"errors"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
)

type UserRepo interface {
	Get(ctx context.Context, id string) (model.User, error)
}

type User struct {
	users UserRepo
}

func NewUser(users UserRepo) *User { return &User{users: users} }

func (s *User) Get(ctx context.Context, id string) (model.User, error) {
	u, err := s.users.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.User{}, ErrUserNotFound
		}
		return model.User{}, err
	}
	return u, nil
}
