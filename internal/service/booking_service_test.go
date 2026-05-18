package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
)

// --- Mocks ---------------------------------------------------------------

type mockBookingRepo struct {
	getFn           func(ctx context.Context, id string) (model.Booking, error)
	createCheckedFn func(ctx context.Context, b model.Booking) (*model.Booking, error)
	cancelFn        func(ctx context.Context, id string) error
	listByUserFn    func(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error)
}

func (m *mockBookingRepo) Get(ctx context.Context, id string) (model.Booking, error) {
	if m.getFn == nil {
		panic("mockBookingRepo.Get: not set up")
	}
	return m.getFn(ctx, id)
}

func (m *mockBookingRepo) CreateChecked(ctx context.Context, b model.Booking) (*model.Booking, error) {
	if m.createCheckedFn == nil {
		panic("mockBookingRepo.CreateChecked: not set up")
	}
	return m.createCheckedFn(ctx, b)
}

func (m *mockBookingRepo) Cancel(ctx context.Context, id string) error {
	if m.cancelFn == nil {
		panic("mockBookingRepo.Cancel: not set up")
	}
	return m.cancelFn(ctx, id)
}

func (m *mockBookingRepo) ListByUser(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error) {
	if m.listByUserFn == nil {
		panic("mockBookingRepo.ListByUser: not set up")
	}
	return m.listByUserFn(ctx, f)
}

type mockRoomLookup struct {
	getFn func(ctx context.Context, id string) (model.Room, error)
}

func (m *mockRoomLookup) Get(ctx context.Context, id string) (model.Room, error) {
	if m.getFn == nil {
		panic("mockRoomLookup.Get: not set up")
	}
	return m.getFn(ctx, id)
}

// --- Fixtures ------------------------------------------------------------

const (
	testRoomID = "room-1"
	testUserID = "user-1"
)

var (
	fixedNow  = time.Date(2026, 5, 19, 8, 0, 0, 0, time.UTC)
	baseStart = time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	baseEnd   = baseStart.Add(1 * time.Hour) // 10:00–11:00
)

func testRoom(floor int) model.Room {
	return model.Room{ID: testRoomID, Name: "Room 1", Capacity: 6, Floor: floor, Status: model.RoomStatusActive}
}

// roomFoundWithStatus — комната существует с произвольным статусом.
func roomFoundWithStatus(rooms *mockRoomLookup, floor int, status model.RoomStatus) {
	rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
		r := testRoom(floor)
		r.Status = status
		return r, nil
	}
}

func testActor(role model.Role) Actor {
	a := Actor{ID: testUserID, Role: role}
	if role == model.RoleManager {
		f := 2
		a.ManagesFloor = &f
	}
	return a
}

func testInput(start, end time.Time) BookingCreateInput {
	return BookingCreateInput{
		RoomID:    testRoomID,
		Title:     "Standup",
		StartTime: start,
		EndTime:   end,
	}
}

func newTestService(rooms *mockRoomLookup, repo *mockBookingRepo) *Booking {
	s := NewBooking(rooms, repo)
	s.now = func() time.Time { return fixedNow }
	return s
}

// roomFound — типовой setup: комната существует с заданным этажом.
func roomFound(rooms *mockRoomLookup, floor int) {
	rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
		return testRoom(floor), nil
	}
}

// noConflictInsert — типовой setup: вставка прошла без конфликта.
func noConflictInsert(repo *mockBookingRepo) {
	repo.createCheckedFn = func(_ context.Context, _ model.Booking) (*model.Booking, error) {
		return nil, nil
	}
}

// --- Tests ---------------------------------------------------------------

func TestBookingService_Create(t *testing.T) {
	type testCase struct {
		name           string
		input          BookingCreateInput
		actor          Actor
		setupMocks     func(rooms *mockRoomLookup, repo *mockBookingRepo)
		wantErrIs      error // сравнение через errors.Is (sentinel)
		wantErrAs      any   // сравнение через errors.As (типизированные ошибки)
		wantHTTPStatus int   // справочно: соответствие из таблицы сценариев
		skipReason     string
	}

	cases := []testCase{
		// --- Happy path ---
		{
			name:  "TC-001 member books free room for 1 hour",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:  "TC-002 manager books room on own floor",
			actor: testActor(model.RoleManager),
			input: testInput(baseStart, baseStart.Add(90*time.Minute)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2) // manager.ManagesFloor == 2
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:  "TC-003 admin books any room",
			actor: testActor(model.RoleAdmin),
			input: testInput(baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 5)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},

		// --- Duration boundaries ---
		{
			name:  "TC-004 exactly 15 minutes (lower bound) ok",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseStart.Add(15*time.Minute)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:           "TC-005 14 minutes (below min) rejected",
			actor:          testActor(model.RoleMember),
			input:          testInput(baseStart, baseStart.Add(14*time.Minute)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {}, // not reached
			wantErrIs:      ErrDurationTooShort,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-006 exactly 8 hours (upper bound) ok",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseStart.Add(8*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:           "TC-007 8h1m (above max) rejected",
			actor:          testActor(model.RoleMember),
			input:          testInput(baseStart, baseStart.Add(8*time.Hour+time.Minute)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrDurationTooLong,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-008 admin also bound by duration min",
			actor:          testActor(model.RoleAdmin),
			input:          testInput(baseStart, baseStart.Add(14*time.Minute)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrDurationTooShort,
			wantHTTPStatus: http.StatusBadRequest,
		},

		// --- Conflicts ---
		{
			name:  "TC-009 full overlap",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(baseStart, baseEnd)
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-010 partial overlap (start inside existing)",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart.Add(time.Hour), baseStart.Add(3*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(baseStart, baseStart.Add(2*time.Hour))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-011 partial overlap (end inside existing)",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart.Add(-time.Hour), baseStart.Add(time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(baseStart, baseStart.Add(2*time.Hour))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-012 request fully inside existing",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart.Add(30*time.Minute), baseStart.Add(90*time.Minute)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(baseStart, baseStart.Add(2*time.Hour))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-013 request fully envelops existing",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseStart.Add(2*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(baseStart.Add(30*time.Minute), baseStart.Add(90*time.Minute))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-014 boundary touch: new starts exactly when old ends",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart.Add(time.Hour), baseStart.Add(2*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo) // SQL `start < $end AND end > $start` исключает касание
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:  "TC-015 boundary touch: new ends exactly when old starts",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseStart.Add(time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},

		// --- Required fields / time range ---
		{
			name:  "TC-016 empty room_id",
			actor: testActor(model.RoleMember),
			input: BookingCreateInput{
				RoomID: "  ", Title: "Standup", StartTime: baseStart, EndTime: baseEnd,
			},
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-017 zero start_time",
			actor: testActor(model.RoleMember),
			input: BookingCreateInput{
				RoomID: testRoomID, Title: "Standup", StartTime: time.Time{}, EndTime: baseEnd,
			},
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-018 zero end_time",
			actor: testActor(model.RoleMember),
			input: BookingCreateInput{
				RoomID: testRoomID, Title: "Standup", StartTime: baseStart, EndTime: time.Time{},
			},
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-019 empty title",
			actor: testActor(model.RoleMember),
			input: BookingCreateInput{
				RoomID: testRoomID, Title: "   ", StartTime: baseStart, EndTime: baseEnd,
			},
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-020 start_time in the past",
			actor:          testActor(model.RoleMember),
			input:          testInput(fixedNow.Add(-24*time.Hour), fixedNow.Add(-23*time.Hour)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrStartInPast,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-021 end_time == start_time",
			actor:          testActor(model.RoleMember),
			input:          testInput(baseStart, baseStart),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrInvalidTimeRange,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-022 end_time before start_time",
			actor:          testActor(model.RoleMember),
			input:          testInput(baseEnd, baseStart),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrInvalidTimeRange,
			wantHTTPStatus: http.StatusBadRequest,
		},

		// --- Room availability ---
		{
			name:  "TC-023 room not found",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, _ *mockBookingRepo) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
					return model.Room{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrRoomNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name:  "TC-024 soft-deleted room (repo treats as not found)",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, _ *mockBookingRepo) {
				// Предположение: room-repo фильтрует soft-deleted и возвращает ErrNotFound.
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
					return model.Room{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrRoomNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name:  "TC-025 room out_of_service",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, _ *mockBookingRepo) {
				roomFoundWithStatus(rooms, 2, model.RoomStatusOutOfService)
			},
			wantErrIs:      ErrRoomOutOfService,
			wantHTTPStatus: http.StatusConflict,
		},

		// --- AuthN / race ---
		{
			name:           "TC-026 unauthenticated request",
			actor:          Actor{},
			input:          testInput(baseStart, baseEnd),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantHTTPStatus: http.StatusUnauthorized,
			skipReason:     "authentication enforced at handler.authMiddleware, not service — covered by handler tests",
		},
		{
			name:  "TC-027 race: conflict reported at insert time",
			actor: testActor(model.RoleMember),
			input: testInput(baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				// Unit-аппроксимация: репозиторий-уровень атомарно обнаружил конфликт.
				// Полноценный race-сценарий — интеграционный тест с реальной БД.
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(baseStart, baseEnd)
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipReason != "" {
				t.Skipf("skip (HTTP %d expected): %s", tc.wantHTTPStatus, tc.skipReason)
			}

			rooms := &mockRoomLookup{}
			repo := &mockBookingRepo{}
			tc.setupMocks(rooms, repo)

			svc := newTestService(rooms, repo)
			got, err := svc.Create(context.Background(), tc.actor, tc.input)

			switch {
			case tc.wantErrIs != nil:
				assert.ErrorIs(t, err, tc.wantErrIs, "expected sentinel error")
				assert.Empty(t, got.ID)
			case tc.wantErrAs != nil:
				assert.Error(t, err)
				assert.True(t, errors.As(err, tc.wantErrAs),
					"expected typed error %T, got %T (%v)", tc.wantErrAs, err, err)
				assert.Empty(t, got.ID)
			default:
				assert.NoError(t, err)
				assert.NotEmpty(t, got.ID, "expected booking id to be assigned")
				assert.Equal(t, tc.input.RoomID, got.RoomID)
				assert.Equal(t, tc.actor.ID, got.UserID)
				assert.Equal(t, model.StatusConfirmed, got.Status)
				assert.True(t, got.StartTime.Equal(tc.input.StartTime.UTC()))
				assert.True(t, got.EndTime.Equal(tc.input.EndTime.UTC()))
			}
		})
	}
}

// conflictAt — фабрика мок-функции CreateChecked, эмулирующей обнаруженный конфликт.
func conflictAt(start, end time.Time) func(context.Context, model.Booking) (*model.Booking, error) {
	return func(_ context.Context, _ model.Booking) (*model.Booking, error) {
		return &model.Booking{
			ID:        "b-existing",
			RoomID:    testRoomID,
			UserID:    "user-other",
			Title:     "existing",
			StartTime: start,
			EndTime:   end,
			Status:    model.StatusConfirmed,
		}, nil
	}
}
