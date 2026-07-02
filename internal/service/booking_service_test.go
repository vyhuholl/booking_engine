package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/cache"
	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/testutil"
)

// --- Mocks ---------------------------------------------------------------

type mockBookingRepo struct {
	getFn            func(ctx context.Context, id string) (model.Booking, error)
	createCheckedFn  func(ctx context.Context, b model.Booking) (*model.Booking, error)
	cancelFn         func(ctx context.Context, id string) error
	cancelAndOfferFn func(ctx context.Context, id string, now time.Time) (model.Booking, *model.WaitlistEntry, error)
	isRoomBusyFn     func(ctx context.Context, roomID string, start, end time.Time) (bool, error)
	listByUserFn     func(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error)
	listByRoomFn     func(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error)
	getByRangeFn     func(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error)
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

func (m *mockBookingRepo) CancelAndOfferWaitlist(ctx context.Context, id string, now time.Time) (model.Booking, *model.WaitlistEntry, error) {
	if m.cancelAndOfferFn == nil {
		panic("mockBookingRepo.CancelAndOfferWaitlist: not set up")
	}
	return m.cancelAndOfferFn(ctx, id, now)
}

func (m *mockBookingRepo) IsRoomBusy(ctx context.Context, roomID string, start, end time.Time) (bool, error) {
	if m.isRoomBusyFn == nil {
		panic("mockBookingRepo.IsRoomBusy: not set up")
	}
	return m.isRoomBusyFn(ctx, roomID, start, end)
}

func (m *mockBookingRepo) ListByUser(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error) {
	if m.listByUserFn == nil {
		panic("mockBookingRepo.ListByUser: not set up")
	}
	return m.listByUserFn(ctx, f)
}

func (m *mockBookingRepo) ListByRoomInPeriod(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error) {
	if m.listByRoomFn == nil {
		panic("mockBookingRepo.ListByRoomInPeriod: not set up")
	}
	return m.listByRoomFn(ctx, roomID, from, to)
}

func (m *mockBookingRepo) GetBookingsByDateRange(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error) {
	if m.getByRangeFn == nil {
		panic("mockBookingRepo.GetBookingsByDateRange: not set up")
	}
	return m.getByRangeFn(ctx, roomID, from, to)
}

type mockRoomLookup struct {
	getFn       func(ctx context.Context, id string) (model.Room, error)
	availableFn func(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error)
}

func (m *mockRoomLookup) Get(ctx context.Context, id string) (model.Room, error) {
	if m.getFn == nil {
		panic("mockRoomLookup.Get: not set up")
	}
	return m.getFn(ctx, id)
}

func (m *mockRoomLookup) Available(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error) {
	if m.availableFn == nil {
		panic("mockRoomLookup.Available: not set up")
	}
	return m.availableFn(ctx, start, end, capacityMin, floor, equipment)
}

// mockRoomCache — ручной мок RoomCacheInterface. Как и репозиторные моки,
// паникует на незаданном методе: это ловит вызовы, которых тест не ожидал
// (напр. поход в кэш, когда должен был сработать быстрый путь).
type mockRoomCache struct {
	getFn        func(ctx context.Context, start, end time.Time) ([]model.Room, error)
	setFn        func(ctx context.Context, start, end time.Time, rooms []model.Room) error
	invalidateFn func(ctx context.Context, roomID string) error
}

func (m *mockRoomCache) GetAvailableRooms(ctx context.Context, start, end time.Time) ([]model.Room, error) {
	if m.getFn == nil {
		panic("mockRoomCache.GetAvailableRooms: not set up")
	}
	return m.getFn(ctx, start, end)
}

func (m *mockRoomCache) SetAvailableRooms(ctx context.Context, start, end time.Time, rooms []model.Room) error {
	if m.setFn == nil {
		panic("mockRoomCache.SetAvailableRooms: not set up")
	}
	return m.setFn(ctx, start, end, rooms)
}

func (m *mockRoomCache) InvalidateRoomCache(ctx context.Context, roomID string) error {
	if m.invalidateFn == nil {
		panic("mockRoomCache.InvalidateRoomCache: not set up")
	}
	return m.invalidateFn(ctx, roomID)
}

// mockPublisher — записывающий спай EventPublisher. В отличие от репозиторных
// моков не паникует при незаданном поведении: публикация — ожидаемый побочный
// эффект успешной операции. Если err != nil, Publish возвращает её (имитация
// недоступной шины).
type mockPublisher struct {
	mu        sync.Mutex
	err       error
	published []publishedEvent
}

type publishedEvent struct {
	topic string
	event events.Event
}

func (m *mockPublisher) Publish(_ context.Context, topic string, e events.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published = append(m.published, publishedEvent{topic: topic, event: e})
	return m.err
}

func (m *mockPublisher) calls() []publishedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]publishedEvent(nil), m.published...)
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
	return testutil.Room(testutil.WithFloor(floor))
}

// roomFoundWithStatus — комната существует с произвольным статусом.
func roomFoundWithStatus(rooms *mockRoomLookup, floor int, status model.RoomStatus) {
	rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
		return testutil.Room(testutil.WithFloor(floor), testutil.WithRoomStatus(status)), nil
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

// createInputFrom — извлекает поля BookingCreateInput из готовой брони-фикстуры:
// builder и object-mother отдают model.Booking, а Create принимает DTO.
func createInputFrom(b model.Booking) BookingCreateInput {
	return BookingCreateInput{
		RoomID:    b.RoomID,
		Title:     b.Title,
		StartTime: b.StartTime,
		EndTime:   b.EndTime,
	}
}

// testInput собирает BookingCreateInput через builder брони: явное окно
// [start, end), остальные поля — дефолты фикстуры.
func testInput(t *testing.T, start, end time.Time) BookingCreateInput {
	t.Helper()
	return createInputFrom(testutil.NewBookingBuilder(t).WithTime(start, end).Build())
}

const testTopic = "booking.events"

func newTestService(rooms *mockRoomLookup, repo *mockBookingRepo) *Booking {
	return newTestServiceWithPublisher(rooms, repo, &mockPublisher{})
}

func newTestServiceWithPublisher(rooms *mockRoomLookup, repo *mockBookingRepo, pub events.EventPublisher) *Booking {
	s := NewBooking(rooms, repo, nil, pub, testTopic, nil)
	s.now = func() time.Time { return fixedNow }
	return s
}

// newTestServiceWithCache собирает сервис с заданным кэшем (и записывающим
// паблишером, чтобы публикация не мешала проверкам кэша).
func newTestServiceWithCache(rooms *mockRoomLookup, repo *mockBookingRepo, c cache.RoomCacheInterface) *Booking {
	s := NewBooking(rooms, repo, c, &mockPublisher{}, testTopic, nil)
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
			input: createInputFrom(testutil.TestBooking(t)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:  "TC-002 manager books room on own floor",
			actor: testActor(model.RoleManager),
			input: testInput(t, baseStart, baseStart.Add(90*time.Minute)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2) // manager.ManagesFloor == 2
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:  "TC-003 admin books any room",
			actor: testActor(model.RoleAdmin),
			input: createInputFrom(testutil.TestBooking(t)),
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
			input: testInput(t, baseStart, baseStart.Add(15*time.Minute)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:           "TC-005 14 minutes (below min) rejected",
			actor:          testActor(model.RoleMember),
			input:          testInput(t, baseStart, baseStart.Add(14*time.Minute)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {}, // not reached
			wantErrIs:      ErrDurationTooShort,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-006 exactly 8 hours (upper bound) ok",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart, baseStart.Add(8*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:           "TC-007 8h1m (above max) rejected",
			actor:          testActor(model.RoleMember),
			input:          testInput(t, baseStart, baseStart.Add(8*time.Hour+time.Minute)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrDurationTooLong,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-008 admin also bound by duration min",
			actor:          testActor(model.RoleAdmin),
			input:          testInput(t, baseStart, baseStart.Add(14*time.Minute)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrDurationTooShort,
			wantHTTPStatus: http.StatusBadRequest,
		},

		// --- Conflicts ---
		{
			name:  "TC-009 full overlap",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(t, baseStart, baseEnd)
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-010 partial overlap (start inside existing)",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart.Add(time.Hour), baseStart.Add(3*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(t, baseStart, baseStart.Add(2*time.Hour))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-011 partial overlap (end inside existing)",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart.Add(-time.Hour), baseStart.Add(time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(t, baseStart, baseStart.Add(2*time.Hour))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-012 request fully inside existing",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart.Add(30*time.Minute), baseStart.Add(90*time.Minute)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(t, baseStart, baseStart.Add(2*time.Hour))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-013 request fully envelops existing",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart, baseStart.Add(2*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(t, baseStart.Add(30*time.Minute), baseStart.Add(90*time.Minute))
			},
			wantErrAs:      new(*BookingConflictError),
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-014 boundary touch: new starts exactly when old ends",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart.Add(time.Hour), baseStart.Add(2*time.Hour)),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				roomFound(rooms, 2)
				noConflictInsert(repo) // SQL `start < $end AND end > $start` исключает касание
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:  "TC-015 boundary touch: new ends exactly when old starts",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart, baseStart.Add(time.Hour)),
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
			input:          testInput(t, fixedNow.Add(-24*time.Hour), fixedNow.Add(-23*time.Hour)),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrStartInPast,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-021 end_time == start_time",
			actor:          testActor(model.RoleMember),
			input:          testInput(t, baseStart, baseStart),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrInvalidTimeRange,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-022 end_time before start_time",
			actor:          testActor(model.RoleMember),
			input:          testInput(t, baseEnd, baseStart),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrInvalidTimeRange,
			wantHTTPStatus: http.StatusBadRequest,
		},

		// --- Room availability ---
		{
			name:  "TC-023 room not found",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart, baseEnd),
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
			input: testInput(t, baseStart, baseEnd),
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
			input: testInput(t, baseStart, baseEnd),
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
			input:          testInput(t, baseStart, baseEnd),
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantHTTPStatus: http.StatusUnauthorized,
			skipReason:     "authentication enforced at handler.authMiddleware, not service — covered by handler tests",
		},
		{
			name:  "TC-027 race: conflict reported at insert time",
			actor: testActor(model.RoleMember),
			input: testInput(t, baseStart, baseEnd),
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				// Unit-аппроксимация: репозиторий-уровень атомарно обнаружил конфликт.
				// Полноценный race-сценарий — интеграционный тест с реальной БД.
				roomFound(rooms, 2)
				repo.createCheckedFn = conflictAt(t, baseStart, baseEnd)
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
// Существующую бронь на слот [start, end) собираем builder'ом, а «претендента» на
// тот же слот — object-mother'ом TestConflictingBooking (та же комната и окно →
// гарантированное пересечение).
func conflictAt(t *testing.T, start, end time.Time) func(context.Context, model.Booking) (*model.Booking, error) {
	t.Helper()
	existing := testutil.NewBookingBuilder(t).
		WithRoom(testutil.Room(testutil.WithRoomID(testRoomID))).
		WithTime(start, end).
		Build()
	conflict := testutil.TestConflictingBooking(t, existing)
	return func(_ context.Context, _ model.Booking) (*model.Booking, error) {
		return &conflict, nil
	}
}

// --- Cancel / ListByUser fixtures ---------------------------------------

const (
	testBookingID = "b-1"
	testOtherID   = "user-other"
	testAdminID   = "admin-1"
)

// errAny — произвольная не-sentinel ошибка репозитория (для проверки проброса).
var errAny = errors.New("unexpected repository failure")

// testBooking — бронь с заданным владельцем, началом и статусом (длительность 1 час),
// собранная builder'ом. ID закрепляем за testBookingID: Cancel сверяет got.ID с ним.
func testBooking(t *testing.T, owner string, start time.Time, status model.BookingStatus) model.Booking {
	t.Helper()
	b := testutil.NewBookingBuilder(t).
		WithUser(testutil.User(testutil.WithUserID(owner))).
		WithTime(start, start.Add(time.Hour)).
		WithStatus(string(status)).
		Build()
	b.ID = testBookingID
	return b
}

// bookingGet — repo.Get возвращает заданную бронь.
func bookingGet(repo *mockBookingRepo, b model.Booking) {
	repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
		return b, nil
	}
}

// cancelOK — repo.CancelAndOfferWaitlist отменяет бронь успешно и никого не
// предлагает из очереди (offered == nil). Возвращает ту же бронь, что отдаёт getFn
// (её задаёт bookingGet до вызова), со статусом cancelled — как реальный репозиторий.
func cancelOK(repo *mockBookingRepo) {
	repo.cancelAndOfferFn = func(ctx context.Context, id string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
		b, err := repo.getFn(ctx, id)
		if err != nil {
			return model.Booking{}, nil, err
		}
		b.Status = model.StatusCancelled
		return b, nil, nil
	}
}

// --- Cancel tests --------------------------------------------------------

func TestBookingService_Cancel(t *testing.T) {
	type testCase struct {
		name           string
		actor          Actor
		bookingID      string
		setupMocks     func(rooms *mockRoomLookup, repo *mockBookingRepo)
		wantErrIs      error
		wantHTTPStatus int
		skipReason     string
	}

	cases := []testCase{
		// --- Happy path / deadline boundary ---
		{
			name:      "TC-028 member cancels own booking 2h before start",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testUserID, baseStart, model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "TC-029 cancel exactly 30 minutes before start (boundary)",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testUserID, fixedNow.Add(CancelDeadline), model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "TC-030 cancel 29 minutes before start rejected",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testUserID, fixedNow.Add(29*time.Minute), model.StatusConfirmed))
			},
			wantErrIs:      ErrCancelTooLate,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:      "TC-031 cancel after booking already started rejected",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testUserID, fixedNow.Add(-15*time.Minute), model.StatusConfirmed))
			},
			wantErrIs:      ErrCancelTooLate,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:      "TC-032 30-minute boundary computed in UTC regardless of start's zone",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				// Абсолютный момент == now+30m, но хранится в не-UTC зоне.
				msk := time.FixedZone("MSK", 3*60*60)
				start := fixedNow.Add(CancelDeadline).In(msk)
				bookingGet(repo, testBooking(t, testUserID, start, model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},

		// --- Authorization ---
		{
			name:      "TC-033 member cannot cancel another user's booking",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
			},
			wantErrIs:      ErrCancelForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:      "TC-034 manager cancels another user's booking on own floor",
			actor:     testActor(model.RoleManager), // ManagesFloor == 2
			bookingID: testBookingID,
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
				roomFound(rooms, 2)
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "TC-035 manager cannot cancel another user's booking on other floor",
			actor:     testActor(model.RoleManager),
			bookingID: testBookingID,
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
				roomFound(rooms, 3)
			},
			wantErrIs:      ErrCancelForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:      "TC-036 manager cancels own booking on other floor (as owner)",
			actor:     testActor(model.RoleManager),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				// Владелец — сам менеджер: room lookup не требуется.
				bookingGet(repo, testBooking(t, testUserID, baseStart, model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "TC-037 manager cancels admin's booking on own floor",
			actor:     testActor(model.RoleManager),
			bookingID: testBookingID,
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testAdminID, baseStart, model.StatusConfirmed))
				roomFound(rooms, 2)
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "TC-038 admin cancels any booking on any floor",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "TC-039 admin still bound by 30-minute rule",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, fixedNow.Add(20*time.Minute), model.StatusConfirmed))
			},
			wantErrIs:      ErrCancelTooLate,
			wantHTTPStatus: http.StatusBadRequest,
		},

		// --- Not found / already cancelled / auth ---
		{
			name:      "TC-040 cancel non-existent booking",
			actor:     testActor(model.RoleMember),
			bookingID: "ghost-999",
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
					return model.Booking{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrBookingNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name:      "TC-041 cancel an already cancelled booking",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testUserID, baseStart, model.StatusCancelled))
			},
			wantErrIs:      ErrAlreadyCancelled,
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:           "TC-042 empty booking_id",
			actor:          testActor(model.RoleMember),
			bookingID:      "",
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantHTTPStatus: http.StatusBadRequest,
			skipReason:     "booking_id is a chi path param; an empty id never routes to the service — enforced at handler/router",
		},
		{
			name:           "TC-043 unauthenticated cancel request",
			actor:          Actor{},
			bookingID:      testBookingID,
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantHTTPStatus: http.StatusUnauthorized,
			skipReason:     "authentication enforced at handler.authMiddleware, not service — covered by handler tests",
		},

		// --- New branch coverage (beyond the original CancelBooking table) ---
		{
			name:      "TC-044 manager without assigned floor cannot cancel another user's booking",
			actor:     Actor{ID: testUserID, Role: model.RoleManager}, // ManagesFloor == nil
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
			},
			wantErrIs:      ErrCancelForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:      "TC-045 manager denied when the booking's room lookup fails",
			actor:     testActor(model.RoleManager),
			bookingID: testBookingID,
			setupMocks: func(rooms *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
					return model.Room{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrCancelForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:      "TC-046 race: booking cancelled between Get and Cancel",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testUserID, baseStart, model.StatusConfirmed))
				repo.cancelAndOfferFn = func(_ context.Context, _ string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
					return model.Booking{}, nil, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrAlreadyCancelled,
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:      "TC-047 repository Get failure is propagated",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
					return model.Booking{}, errAny
				}
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
		{
			name:      "TC-048 repository Cancel failure is propagated",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testUserID, baseStart, model.StatusConfirmed))
				repo.cancelAndOfferFn = func(_ context.Context, _ string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
					return model.Booking{}, nil, errAny
				}
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
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
			got, err := svc.Cancel(context.Background(), tc.actor, tc.bookingID)

			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs, "expected sentinel error")
				assert.Equal(t, model.Booking{}, got, "no booking returned on error")
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tc.bookingID, got.ID)
			assert.Equal(t, model.StatusCancelled, got.Status, "booking marked cancelled")
		})
	}
}

// --- ForceCancel tests ---------------------------------------------------

// TestBookingService_ForceCancel покрывает принудительную отмену админом. Ключевые
// отличия от Cancel: доступно только admin и дедлайн CancelDeadline не применяется
// (можно отменить уже начавшуюся бронь). Общие ветки (not found / already cancelled /
// гонка / проброс ошибок репозитория) проверяются так же, как в TestBookingService_Cancel.
func TestBookingService_ForceCancel(t *testing.T) {
	type testCase struct {
		name           string
		actor          Actor
		bookingID      string
		setupMocks     func(rooms *mockRoomLookup, repo *mockBookingRepo)
		wantErrIs      error
		wantHTTPStatus int
	}

	cases := []testCase{
		// --- Happy path: admin, дедлайн игнорируется ---
		{
			name:      "admin force-cancels another user's booking 2h before start",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "admin force-cancels within the 30-minute deadline (bypassed)",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				// Обычный Cancel вернул бы ErrCancelTooLate (TC-030); force его игнорирует.
				bookingGet(repo, testBooking(t, testOtherID, fixedNow.Add(20*time.Minute), model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:      "admin force-cancels an already started booking (deadline bypassed)",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, fixedNow.Add(-15*time.Minute), model.StatusConfirmed))
				cancelOK(repo)
			},
			wantHTTPStatus: http.StatusNoContent,
		},

		// --- Authorization: только admin ---
		{
			name:      "member cannot force-cancel even own booking",
			actor:     testActor(model.RoleMember),
			bookingID: testBookingID,
			// getFn не задан: проверка прав идёт до похода в БД, репозиторий не трогается.
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:           "manager cannot force-cancel even on own floor",
			actor:          testActor(model.RoleManager), // ManagesFloor == 2
			bookingID:      testBookingID,
			setupMocks:     func(*mockRoomLookup, *mockBookingRepo) {},
			wantErrIs:      ErrForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},

		// --- Not found / already cancelled / гонка / проброс ошибок ---
		{
			name:      "force-cancel non-existent booking",
			actor:     testActor(model.RoleAdmin),
			bookingID: "ghost-999",
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
					return model.Booking{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrBookingNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name:      "force-cancel an already cancelled booking",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusCancelled))
			},
			wantErrIs:      ErrAlreadyCancelled,
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:      "race: booking cancelled between Get and Cancel",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
				repo.cancelAndOfferFn = func(_ context.Context, _ string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
					return model.Booking{}, nil, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrAlreadyCancelled,
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:      "repository Get failure is propagated",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
					return model.Booking{}, errAny
				}
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
		{
			name:      "repository Cancel failure is propagated",
			actor:     testActor(model.RoleAdmin),
			bookingID: testBookingID,
			setupMocks: func(_ *mockRoomLookup, repo *mockBookingRepo) {
				bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))
				repo.cancelAndOfferFn = func(_ context.Context, _ string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
					return model.Booking{}, nil, errAny
				}
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rooms := &mockRoomLookup{}
			repo := &mockBookingRepo{}
			tc.setupMocks(rooms, repo)

			svc := newTestService(rooms, repo)
			got, err := svc.ForceCancel(context.Background(), tc.actor, tc.bookingID)

			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs, "expected sentinel error")
				assert.Equal(t, model.Booking{}, got, "no booking returned on error")
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tc.bookingID, got.ID)
			assert.Equal(t, model.StatusCancelled, got.Status, "booking marked cancelled")
		})
	}
}

// TestBookingService_ForceCancel_PublishesEvent: успешная принудительная отмена
// публикует ровно одно событие booking.cancelled (тот же контракт, что у Cancel).
func TestBookingService_ForceCancel_PublishesEvent(t *testing.T) {
	repo := &mockBookingRepo{}
	b := testBooking(t, testOtherID, baseStart, model.StatusConfirmed)
	bookingGet(repo, b)
	cancelOK(repo)

	pub := &mockPublisher{}
	svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, pub)

	_, err := svc.ForceCancel(context.Background(), testActor(model.RoleAdmin), testBookingID)
	assert.NoError(t, err)

	calls := pub.calls()
	assert.Len(t, calls, 1)
	assert.Equal(t, events.TypeBookingCancelled, calls[0].event.Type)
	assert.Equal(t, b.ID, calls[0].event.BookingID)
}

// TestBookingService_ForceCancel_NoEventOnForbidden: отклонённая по правам
// принудительная отмена не публикует событие и не трогает репозиторий.
func TestBookingService_ForceCancel_NoEventOnForbidden(t *testing.T) {
	pub := &mockPublisher{}
	// Ни репозиторий, ни кэш не заданы → любой их вызов паникует: проверяем,
	// что не-админ отсекается до всякого обращения к ним.
	svc := newTestServiceWithPublisher(&mockRoomLookup{}, &mockBookingRepo{}, pub)

	_, err := svc.ForceCancel(context.Background(), testActor(model.RoleMember), testBookingID)
	assert.ErrorIs(t, err, ErrForbidden)
	assert.Empty(t, pub.calls(), "no event on forbidden force-cancel")
}

// --- Event publishing tests ---------------------------------------------

// TestBookingService_Create_PublishesEvent: успешный Create публикует ровно
// одно событие booking.created в нужный топик с полями брони.
func TestBookingService_Create_PublishesEvent(t *testing.T) {
	rooms := &mockRoomLookup{}
	repo := &mockBookingRepo{}
	roomFound(rooms, 2)
	noConflictInsert(repo)

	pub := &mockPublisher{}
	svc := newTestServiceWithPublisher(rooms, repo, pub)

	actor := testActor(model.RoleMember)
	got, err := svc.Create(context.Background(), actor, testInput(t, baseStart, baseEnd))
	assert.NoError(t, err)

	calls := pub.calls()
	assert.Len(t, calls, 1, "exactly one event published")
	assert.Equal(t, testTopic, calls[0].topic)
	ev := calls[0].event
	assert.Equal(t, events.TypeBookingCreated, ev.Type)
	assert.Equal(t, got.ID, ev.BookingID)
	assert.Equal(t, actor.ID, ev.UserID)
	assert.Equal(t, got.RoomID, ev.RoomID)
	assert.True(t, ev.Timestamp.Equal(fixedNow), "timestamp taken from service clock")
}

// TestBookingService_Create_PublishFailureDoesNotFail: сбой публикации не
// откатывает бронь — Create возвращает её без ошибки (eventual consistency).
func TestBookingService_Create_PublishFailureDoesNotFail(t *testing.T) {
	rooms := &mockRoomLookup{}
	repo := &mockBookingRepo{}
	roomFound(rooms, 2)
	noConflictInsert(repo)

	pub := &mockPublisher{err: errAny}
	svc := newTestServiceWithPublisher(rooms, repo, pub)

	got, err := svc.Create(context.Background(), testActor(model.RoleMember), testInput(t, baseStart, baseEnd))
	assert.NoError(t, err, "publish failure must not fail the booking")
	assert.NotEmpty(t, got.ID)
	assert.Len(t, pub.calls(), 1, "publish was attempted")
}

// TestBookingService_Create_NoEventOnFailure: если бронь не создана
// (валидация/конфликт), событие не публикуется.
func TestBookingService_Create_NoEventOnFailure(t *testing.T) {
	rooms := &mockRoomLookup{}
	repo := &mockBookingRepo{}
	roomFound(rooms, 2)
	repo.createCheckedFn = conflictAt(t, baseStart, baseEnd)

	pub := &mockPublisher{}
	svc := newTestServiceWithPublisher(rooms, repo, pub)

	_, err := svc.Create(context.Background(), testActor(model.RoleMember), testInput(t, baseStart, baseEnd))
	assert.Error(t, err)
	assert.Empty(t, pub.calls(), "no event on failed create")
}

// TestBookingService_Cancel_PublishesEvent: успешный Cancel публикует ровно
// одно событие booking.cancelled.
func TestBookingService_Cancel_PublishesEvent(t *testing.T) {
	repo := &mockBookingRepo{}
	b := testBooking(t, testUserID, baseStart, model.StatusConfirmed)
	bookingGet(repo, b)
	cancelOK(repo)

	pub := &mockPublisher{}
	svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, pub)

	_, err := svc.Cancel(context.Background(), testActor(model.RoleMember), testBookingID)
	assert.NoError(t, err)

	calls := pub.calls()
	assert.Len(t, calls, 1)
	assert.Equal(t, testTopic, calls[0].topic)
	ev := calls[0].event
	assert.Equal(t, events.TypeBookingCancelled, ev.Type)
	assert.Equal(t, b.ID, ev.BookingID)
	assert.Equal(t, b.UserID, ev.UserID)
	assert.Equal(t, b.RoomID, ev.RoomID)
	assert.True(t, ev.Timestamp.Equal(fixedNow))
}

// TestBookingService_Cancel_PublishFailureDoesNotFail: сбой публикации не
// откатывает отмену.
func TestBookingService_Cancel_PublishFailureDoesNotFail(t *testing.T) {
	repo := &mockBookingRepo{}
	bookingGet(repo, testBooking(t, testUserID, baseStart, model.StatusConfirmed))
	cancelOK(repo)

	pub := &mockPublisher{err: errAny}
	svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, pub)

	got, err := svc.Cancel(context.Background(), testActor(model.RoleMember), testBookingID)
	assert.NoError(t, err)
	assert.Equal(t, model.StatusCancelled, got.Status)
	assert.Len(t, pub.calls(), 1, "publish was attempted")
}

// TestBookingService_Cancel_NoEventOnFailure: отклонённая отмена (нет прав)
// не публикует событие.
func TestBookingService_Cancel_NoEventOnFailure(t *testing.T) {
	repo := &mockBookingRepo{}
	bookingGet(repo, testBooking(t, testOtherID, baseStart, model.StatusConfirmed))

	pub := &mockPublisher{}
	svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, pub)

	_, err := svc.Cancel(context.Background(), testActor(model.RoleMember), testBookingID)
	assert.ErrorIs(t, err, ErrCancelForbidden)
	assert.Empty(t, pub.calls(), "no event on forbidden cancel")
}

// --- ListByUser tests ----------------------------------------------------

func TestBookingService_ListByUser(t *testing.T) {
	confirmed := model.StatusConfirmed
	from := baseStart.Add(-24 * time.Hour)
	to := baseStart.Add(24 * time.Hour)
	sample := []model.Booking{testutil.TestBooking(t)}

	type testCase struct {
		name           string
		actor          Actor
		query          UserBookingsQuery
		setupMocks     func(repo *mockBookingRepo)
		wantErrIs      error
		wantLen        int
		wantFilter     *repository.UserBookingFilter
		wantHTTPStatus int
	}

	listReturns := func(out []model.Booking) func(repo *mockBookingRepo) {
		return func(repo *mockBookingRepo) {
			repo.listByUserFn = func(_ context.Context, _ repository.UserBookingFilter) ([]model.Booking, error) {
				return out, nil
			}
		}
	}

	cases := []testCase{
		{
			name:           "TC-049 member lists own bookings",
			actor:          testActor(model.RoleMember),
			query:          UserBookingsQuery{UserID: testUserID},
			setupMocks:     listReturns(sample),
			wantLen:        1,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "TC-050 member cannot list another user's bookings",
			actor:          testActor(model.RoleMember),
			query:          UserBookingsQuery{UserID: testOtherID},
			setupMocks:     func(*mockBookingRepo) {},
			wantErrIs:      ErrForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:           "TC-051 admin lists another user's bookings",
			actor:          testActor(model.RoleAdmin),
			query:          UserBookingsQuery{UserID: testOtherID},
			setupMocks:     listReturns(sample),
			wantLen:        1,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "TC-052 manager lists another user's bookings",
			actor:          testActor(model.RoleManager),
			query:          UserBookingsQuery{UserID: testOtherID},
			setupMocks:     listReturns(nil),
			wantLen:        0,
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "TC-053 filters (status/from/to) forwarded to the repository",
			actor:          testActor(model.RoleMember),
			query:          UserBookingsQuery{UserID: testUserID, Status: &confirmed, From: &from, To: &to},
			setupMocks:     listReturns(sample),
			wantLen:        1,
			wantFilter:     &repository.UserBookingFilter{UserID: testUserID, Status: &confirmed, From: &from, To: &to},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:  "TC-054 repository error is propagated",
			actor: testActor(model.RoleMember),
			query: UserBookingsQuery{UserID: testUserID},
			setupMocks: func(repo *mockBookingRepo) {
				repo.listByUserFn = func(_ context.Context, _ repository.UserBookingFilter) ([]model.Booking, error) {
					return nil, errAny
				}
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockBookingRepo{}
			tc.setupMocks(repo)

			// Оборачиваем ListByUser, чтобы зафиксировать переданный фильтр.
			var gotFilter repository.UserBookingFilter
			if orig := repo.listByUserFn; orig != nil {
				repo.listByUserFn = func(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error) {
					gotFilter = f
					return orig(ctx, f)
				}
			}

			svc := newTestService(&mockRoomLookup{}, repo)
			got, err := svc.ListByUser(context.Background(), tc.actor, tc.query)

			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs)
				assert.Nil(t, got)
				return
			}

			assert.NoError(t, err)
			assert.Len(t, got, tc.wantLen)
			assert.Equal(t, tc.query.UserID, gotFilter.UserID, "user id forwarded to filter")
			if tc.wantFilter != nil {
				assert.Equal(t, *tc.wantFilter, gotFilter)
			}
		})
	}
}

// --- Room availability cache --------------------------------------------

// TestBookingService_GetAvailableRooms проверяет путь кэш → БД → кэш и его
// деградацию: промах и ошибки кэша уводят в БД, но не роняют запрос.
func TestBookingService_GetAvailableRooms(t *testing.T) {
	start, end := baseStart, baseEnd
	sample := []model.Room{testutil.Room()}

	// availableReturns настраивает БД-источник и сверяет переданное окно (UTC RFC3339)
	// и то, что фильтры не заданы (метод берёт только временное окно).
	availableReturns := func(out []model.Room, err error) func(*mockRoomLookup) {
		return func(rooms *mockRoomLookup) {
			rooms.availableFn = func(_ context.Context, s, e string, capMin, floor *int, eq []string) ([]model.Room, error) {
				assert.Equal(t, start.UTC().Format(time.RFC3339), s)
				assert.Equal(t, end.UTC().Format(time.RFC3339), e)
				assert.Nil(t, capMin)
				assert.Nil(t, floor)
				assert.Nil(t, eq)
				return out, err
			}
		}
	}

	t.Run("cache hit returns cached rooms without touching DB", func(t *testing.T) {
		rooms := &mockRoomLookup{} // availableFn не задан → паника, если пойдём в БД
		c := &mockRoomCache{
			getFn: func(_ context.Context, s, e time.Time) ([]model.Room, error) {
				assert.True(t, s.Equal(start))
				assert.True(t, e.Equal(end))
				return sample, nil
			},
		}
		svc := newTestServiceWithCache(rooms, &mockBookingRepo{}, c)

		got, err := svc.GetAvailableRooms(context.Background(), testActor(model.RoleMember), start, end)
		assert.NoError(t, err)
		assert.Equal(t, sample, got)
	})

	t.Run("cache miss falls through to DB and populates cache", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		availableReturns(sample, nil)(rooms)

		var cached []model.Room
		var setCalled bool
		c := &mockRoomCache{
			getFn: func(_ context.Context, _, _ time.Time) ([]model.Room, error) {
				return nil, cache.ErrCacheMiss
			},
			setFn: func(_ context.Context, s, e time.Time, rs []model.Room) error {
				setCalled = true
				cached = rs
				assert.True(t, s.Equal(start))
				assert.True(t, e.Equal(end))
				return nil
			},
		}
		svc := newTestServiceWithCache(rooms, &mockBookingRepo{}, c)

		got, err := svc.GetAvailableRooms(context.Background(), testActor(model.RoleMember), start, end)
		assert.NoError(t, err)
		assert.Equal(t, sample, got)
		assert.True(t, setCalled, "результат из БД должен быть закэширован")
		assert.Equal(t, sample, cached)
	})

	t.Run("cache get error degrades to DB", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		availableReturns(sample, nil)(rooms)

		c := &mockRoomCache{
			getFn: func(_ context.Context, _, _ time.Time) ([]model.Room, error) {
				return nil, errAny // не ErrCacheMiss: настоящий сбой Redis
			},
			setFn: func(_ context.Context, _, _ time.Time, _ []model.Room) error { return nil },
		}
		svc := newTestServiceWithCache(rooms, &mockBookingRepo{}, c)

		got, err := svc.GetAvailableRooms(context.Background(), testActor(model.RoleMember), start, end)
		assert.NoError(t, err, "сбой кэша не должен ронять запрос")
		assert.Equal(t, sample, got)
	})

	t.Run("cache set error does not fail the request", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		availableReturns(sample, nil)(rooms)

		c := &mockRoomCache{
			getFn: func(_ context.Context, _, _ time.Time) ([]model.Room, error) { return nil, cache.ErrCacheMiss },
			setFn: func(_ context.Context, _, _ time.Time, _ []model.Room) error { return errAny },
		}
		svc := newTestServiceWithCache(rooms, &mockBookingRepo{}, c)

		got, err := svc.GetAvailableRooms(context.Background(), testActor(model.RoleMember), start, end)
		assert.NoError(t, err)
		assert.Equal(t, sample, got)
	})

	t.Run("nil cache goes straight to DB", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		availableReturns(sample, nil)(rooms)
		svc := newTestService(rooms, &mockBookingRepo{}) // кэш == nil

		got, err := svc.GetAvailableRooms(context.Background(), testActor(model.RoleMember), start, end)
		assert.NoError(t, err)
		assert.Equal(t, sample, got)
	})

	t.Run("DB error is propagated and not cached", func(t *testing.T) {
		rooms := &mockRoomLookup{}
		availableReturns(nil, errAny)(rooms)

		c := &mockRoomCache{
			getFn: func(_ context.Context, _, _ time.Time) ([]model.Room, error) { return nil, cache.ErrCacheMiss },
			// setFn не задан → паника, если попытаемся закэшировать ошибку
		}
		svc := newTestServiceWithCache(rooms, &mockBookingRepo{}, c)

		got, err := svc.GetAvailableRooms(context.Background(), testActor(model.RoleMember), start, end)
		assert.ErrorIs(t, err, errAny)
		assert.Nil(t, got)
	})

	t.Run("invalid time range rejected before cache or DB", func(t *testing.T) {
		// Ни кэш, ни БД не заданы → любой их вызов паникует.
		svc := newTestServiceWithCache(&mockRoomLookup{}, &mockBookingRepo{}, &mockRoomCache{})

		_, err := svc.GetAvailableRooms(context.Background(), testActor(model.RoleMember), end, start)
		assert.ErrorIs(t, err, ErrInvalidTimeRange)
	})
}

// TestBookingService_Create_InvalidatesCache: успешный Create сбрасывает кэш
// доступности комнаты брони.
func TestBookingService_Create_InvalidatesCache(t *testing.T) {
	rooms := &mockRoomLookup{}
	roomFound(rooms, 2)
	repo := &mockBookingRepo{}
	noConflictInsert(repo)

	var invalidated []string
	c := &mockRoomCache{
		invalidateFn: func(_ context.Context, roomID string) error {
			invalidated = append(invalidated, roomID)
			return nil
		},
	}
	svc := newTestServiceWithCache(rooms, repo, c)

	got, err := svc.Create(context.Background(), testActor(model.RoleMember), testInput(t, baseStart, baseEnd))
	assert.NoError(t, err)
	assert.Equal(t, []string{got.RoomID}, invalidated, "созданная бронь инвалидирует кэш своей комнаты")
}

// TestBookingService_Cancel_InvalidatesCache: успешный Cancel сбрасывает кэш
// доступности комнаты брони.
func TestBookingService_Cancel_InvalidatesCache(t *testing.T) {
	repo := &mockBookingRepo{}
	b := testBooking(t, testUserID, baseStart, model.StatusConfirmed)
	bookingGet(repo, b)
	cancelOK(repo)

	var invalidated []string
	c := &mockRoomCache{
		invalidateFn: func(_ context.Context, roomID string) error {
			invalidated = append(invalidated, roomID)
			return nil
		},
	}
	svc := newTestServiceWithCache(&mockRoomLookup{}, repo, c)

	_, err := svc.Cancel(context.Background(), testActor(model.RoleMember), testBookingID)
	assert.NoError(t, err)
	assert.Equal(t, []string{b.RoomID}, invalidated)
}

// TestBookingService_Create_NoCacheInvalidationOnFailure: отклонённый Create
// (конфликт) не трогает кэш — invalidateFn не задан, любой вызов паникует.
func TestBookingService_Create_NoCacheInvalidationOnFailure(t *testing.T) {
	rooms := &mockRoomLookup{}
	roomFound(rooms, 2)
	repo := &mockBookingRepo{}
	repo.createCheckedFn = conflictAt(t, baseStart, baseEnd)

	svc := newTestServiceWithCache(rooms, repo, &mockRoomCache{})

	_, err := svc.Create(context.Background(), testActor(model.RoleMember), testInput(t, baseStart, baseEnd))
	assert.Error(t, err)
}

// TestBookingService_Create_CacheInvalidationErrorDoesNotFail: сбой инвалидации
// кэша не откатывает уже зафиксированную бронь (TTL всё равно ограничит
// рассогласование).
func TestBookingService_Create_CacheInvalidationErrorDoesNotFail(t *testing.T) {
	rooms := &mockRoomLookup{}
	roomFound(rooms, 2)
	repo := &mockBookingRepo{}
	noConflictInsert(repo)

	c := &mockRoomCache{
		invalidateFn: func(_ context.Context, _ string) error { return errAny },
	}
	svc := newTestServiceWithCache(rooms, repo, c)

	got, err := svc.Create(context.Background(), testActor(model.RoleMember), testInput(t, baseStart, baseEnd))
	assert.NoError(t, err, "сбой инвалидации кэша не должен ронять бронь")
	assert.NotEmpty(t, got.ID)
}

// --- Error type messages -------------------------------------------------

func TestServiceErrorMessages(t *testing.T) {
	t.Run("booking conflict error has a stable code", func(t *testing.T) {
		err := &BookingConflictError{ConflictingID: "b-x", ConflictingStart: baseStart, ConflictingEnd: baseEnd}
		assert.Equal(t, "booking_conflict", err.Error())
	})
	t.Run("validation error surfaces its message", func(t *testing.T) {
		err := &ValidationError{Field: "title", Message: "title is required"}
		assert.Equal(t, "title is required", err.Error())
	})
}
