package testutil

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// AssertServiceError проверяет доменную ошибку по полям тест-кейса:
// если задан wantIs — сверяет через errors.Is (sentinel);
// иначе если задан wantAs — сверяет через errors.As (типизированная ошибка).
// wantAs должен быть указателем на целевой тип, например new(*ValidationError).
func AssertServiceError(t testing.TB, err error, wantIs error, wantAs any) {
	t.Helper()
	switch {
	case wantIs != nil:
		assert.ErrorIs(t, err, wantIs, "expected sentinel error")
	case wantAs != nil:
		assert.Error(t, err)
		assert.True(t, errors.As(err, wantAs),
			"expected typed error %T, got %T (%v)", wantAs, err, err)
	}
}

// AssertSentinel — тонкая обёртка над assert.ErrorIs со стандартным сообщением.
func AssertSentinel(t testing.TB, err, want error) {
	t.Helper()
	assert.ErrorIs(t, err, want, "expected sentinel error")
}

// AssertTyped утверждает, что err разворачивается (errors.As) в тип E,
// и возвращает совпавшую ошибку для дальнейших проверок её полей.
func AssertTyped[E error](t testing.TB, err error) E {
	t.Helper()
	var target E
	if !errors.As(err, &target) {
		assert.Failf(t, "unexpected error type",
			"expected %T, got %T (%v)", target, err, err)
	}
	return target
}
