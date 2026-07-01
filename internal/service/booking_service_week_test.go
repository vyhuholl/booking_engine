package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/testutil"
)

// ptr — короткий адрес значения для необязательных полей BookingFilters.
func ptr[T any](v T) *T { return &v }

// TestBookingService_GetBookingsByWeek фиксирует контракт недельной выборки
// броней комнаты, сгруппированной по дням: группировка по дню start_time,
// фильтры по статусу и владельцу, ровно 7 дней на выходе и мягкая обработка
// пустой/несуществующей комнаты (пустой отчёт без ошибки). weekStart обязан
// быть понедельником.
//
// Моки — общие ручные структуры из booking_service_test.go (mockBookingRepo,
// mockRoomLookup): пустой mockRoomLookup паникует на любой вызов, чем ловит
// нежелательный room lookup (отчёт не должен ходить за комнатой). Данные для
// периода отдаёт GetBookingsByDateRange; фильтрация и группировка — задача сервиса.
func TestBookingService_GetBookingsByWeek(t *testing.T) {
	// weekStart — понедельник 2026-05-18 (FixedNow приходится на вторник этой недели).
	weekStart := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	// notMonday — вторник той же недели: невалидное начало недели.
	notMonday := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)

	// mkBooking собирает бронь на day-й день недели (окно 10:00–11:00) с заданным
	// владельцем и статусом — builder'ом, в комнате testRoomID.
	mkBooking := func(day int, owner string, status model.BookingStatus) model.Booking {
		start := weekStart.AddDate(0, 0, day).Add(10 * time.Hour)
		return testutil.NewBookingBuilder(t).
			WithRoom(testutil.Room(testutil.WithRoomID(testRoomID))).
			WithUser(testutil.User(testutil.WithUserID(owner))).
			WithTime(start, start.Add(time.Hour)).
			WithStatus(string(status)).
			Build()
	}

	type testCase struct {
		name       string
		weekStart  time.Time
		filters    model.BookingFilters
		staged     []model.Booking  // что вернёт GetBookingsByDateRange (nil для validation-кейсов)
		wantErrAs  any              // *ValidationError для невалидного weekStart
		wantCounts [DaysPerWeek]int // ожидаемое число броней по дням Пн..Вс
	}

	cases := []testCase{
		{
			name:      "TC-055 bookings on different days land in their own day bucket",
			weekStart: weekStart,
			staged: []model.Booking{
				mkBooking(0, testUserID, model.StatusConfirmed),
				mkBooking(2, testUserID, model.StatusConfirmed),
				mkBooking(4, testUserID, model.StatusConfirmed),
			},
			wantCounts: [DaysPerWeek]int{1, 0, 1, 0, 1, 0, 0},
		},
		{
			name:       "TC-056 week with no bookings returns every day empty",
			weekStart:  weekStart,
			staged:     nil,
			wantCounts: [DaysPerWeek]int{0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:      "TC-057 status filter keeps only bookings with that status",
			weekStart: weekStart,
			filters:   model.BookingFilters{Status: ptr(string(model.StatusConfirmed))},
			staged: []model.Booking{
				mkBooking(0, testUserID, model.StatusConfirmed),
				mkBooking(1, testUserID, model.StatusCancelled), // отфильтрована
				mkBooking(2, testUserID, model.StatusConfirmed),
			},
			wantCounts: [DaysPerWeek]int{1, 0, 1, 0, 0, 0, 0},
		},
		{
			name:      "TC-058 user filter keeps only that user's bookings",
			weekStart: weekStart,
			filters:   model.BookingFilters{UserID: ptr(testUserID)},
			staged: []model.Booking{
				mkBooking(0, testUserID, model.StatusConfirmed),
				mkBooking(1, testOtherID, model.StatusConfirmed), // отфильтрована
				mkBooking(3, testUserID, model.StatusConfirmed),
			},
			wantCounts: [DaysPerWeek]int{1, 0, 0, 1, 0, 0, 0},
		},
		{
			name:      "TC-059 status and user filters both must match",
			weekStart: weekStart,
			filters:   model.BookingFilters{Status: ptr(string(model.StatusConfirmed)), UserID: ptr(testUserID)},
			staged: []model.Booking{
				mkBooking(0, testUserID, model.StatusConfirmed),  // оставлена
				mkBooking(0, testOtherID, model.StatusConfirmed), // не тот пользователь
				mkBooking(1, testUserID, model.StatusCancelled),  // не тот статус
				mkBooking(2, testUserID, model.StatusConfirmed),  // оставлена
			},
			wantCounts: [DaysPerWeek]int{1, 0, 1, 0, 0, 0, 0},
		},
		{
			name:       "TC-060 unknown room returns an empty week without error",
			weekStart:  weekStart,
			staged:     nil, // репозиторий не находит броней — комната как будто пуста
			wantCounts: [DaysPerWeek]int{0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:      "TC-061 week not starting on Monday is rejected",
			weekStart: notMonday,
			staged:    nil, // до репозитория не доходим
			wantErrAs: new(*ValidationError),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockBookingRepo{}
			if tc.wantErrAs == nil {
				staged := tc.staged
				wantFrom := tc.weekStart
				wantTo := tc.weekStart.AddDate(0, 0, DaysPerWeek)
				repo.getByRangeFn = func(_ context.Context, roomID string, from, to time.Time) ([]model.Booking, error) {
					assert.Equal(t, testRoomID, roomID, "выборка по запрошенной комнате")
					assert.True(t, from.Equal(wantFrom), "окно стартует с weekStart")
					assert.True(t, to.Equal(wantTo), "окно — ровно неделя [weekStart, +7д)")
					return staged, nil
				}
			}
			// rooms не задан: любой room lookup запаникует — отчёт по несуществующей
			// комнате обязан вернуть пустую неделю, а не ходить за комнатой.
			svc := newTestService(&mockRoomLookup{}, repo)

			got, err := svc.GetBookingsByWeek(context.Background(), testRoomID, tc.weekStart, tc.filters)

			if tc.wantErrAs != nil {
				assert.Error(t, err)
				assert.True(t, errors.As(err, tc.wantErrAs),
					"expected typed error %T, got %T (%v)", tc.wantErrAs, err, err)
				assert.Nil(t, got, "нет результата при ошибке валидации")
				return
			}

			assert.NoError(t, err)
			assert.Len(t, got, DaysPerWeek, "ровно 7 дней на выходе")
			for i, day := range got {
				assert.True(t, day.Date.Equal(tc.weekStart.AddDate(0, 0, i)),
					"день %d начинается с корректной даты", i)
				assert.NotNil(t, day.Bookings, "список броней дня %d не nil даже когда пуст", i)
				assert.Len(t, day.Bookings, tc.wantCounts[i], "число броней в дне %d", i)
			}
		})
	}
}
