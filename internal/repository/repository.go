package repository

import (
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound — нормализованная ошибка "запись отсутствует".
var ErrNotFound = errors.New("not found")

func wrapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
