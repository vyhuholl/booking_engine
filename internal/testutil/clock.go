package testutil

import "time"

// Clock возвращает константные часы для подмены поля now сервисного слоя:
//
//	svc.now = testutil.Clock(testutil.FixedNow)
//
// Присваивание остаётся в package service (поле now неэкспортируемое).
func Clock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}
