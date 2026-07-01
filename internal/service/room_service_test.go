package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
)

// --- Mocks ---------------------------------------------------------------

type mockRoomRepo struct {
	listFn      func(ctx context.Context, f repository.RoomFilter) ([]model.Room, int, error)
	getFn       func(ctx context.Context, id string) (model.Room, error)
	createFn    func(ctx context.Context, r model.Room) error
	updateFn    func(ctx context.Context, r model.Room) error
	deleteFn    func(ctx context.Context, id string) error
	availableFn func(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error)
}

func (m *mockRoomRepo) List(ctx context.Context, f repository.RoomFilter) ([]model.Room, int, error) {
	if m.listFn == nil {
		panic("mockRoomRepo.List: not set up")
	}
	return m.listFn(ctx, f)
}

func (m *mockRoomRepo) Get(ctx context.Context, id string) (model.Room, error) {
	if m.getFn == nil {
		panic("mockRoomRepo.Get: not set up")
	}
	return m.getFn(ctx, id)
}

func (m *mockRoomRepo) Create(ctx context.Context, r model.Room) error {
	if m.createFn == nil {
		panic("mockRoomRepo.Create: not set up")
	}
	return m.createFn(ctx, r)
}

func (m *mockRoomRepo) Update(ctx context.Context, r model.Room) error {
	if m.updateFn == nil {
		panic("mockRoomRepo.Update: not set up")
	}
	return m.updateFn(ctx, r)
}

func (m *mockRoomRepo) Delete(ctx context.Context, id string) error {
	if m.deleteFn == nil {
		panic("mockRoomRepo.Delete: not set up")
	}
	return m.deleteFn(ctx, id)
}

func (m *mockRoomRepo) Available(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error) {
	if m.availableFn == nil {
		panic("mockRoomRepo.Available: not set up")
	}
	return m.availableFn(ctx, start, end, capacityMin, floor, equipment)
}

type mockBookingsForRoom struct {
	hasActiveFn        func(ctx context.Context, roomID string, after time.Time) (bool, error)
	listByRoomOnDateFn func(ctx context.Context, roomID string, date time.Time) ([]model.Booking, error)
	countInPeriodFn    func(ctx context.Context, roomID string, from, to time.Time) (int, error)
}

func (m *mockBookingsForRoom) HasActiveForRoom(ctx context.Context, roomID string, after time.Time) (bool, error) {
	if m.hasActiveFn == nil {
		panic("mockBookingsForRoom.HasActiveForRoom: not set up")
	}
	return m.hasActiveFn(ctx, roomID, after)
}

func (m *mockBookingsForRoom) ListByRoomOnDate(ctx context.Context, roomID string, date time.Time) ([]model.Booking, error) {
	if m.listByRoomOnDateFn == nil {
		panic("mockBookingsForRoom.ListByRoomOnDate: not set up")
	}
	return m.listByRoomOnDateFn(ctx, roomID, date)
}

func (m *mockBookingsForRoom) CountByRoomInPeriod(ctx context.Context, roomID string, from, to time.Time) (int, error) {
	if m.countInPeriodFn == nil {
		panic("mockBookingsForRoom.CountByRoomInPeriod: not set up")
	}
	return m.countInPeriodFn(ctx, roomID, from, to)
}

func newTestRoomService(rooms *mockRoomRepo, bookings *mockBookingsForRoom) *Room {
	s := NewRoom(rooms, bookings)
	s.now = func() time.Time { return fixedNow }
	return s
}

// --- Create --------------------------------------------------------------

func TestRoomService_Create(t *testing.T) {
	type testCase struct {
		name           string
		actor          Actor
		input          RoomCreateInput
		setupMocks     func(rooms *mockRoomRepo)
		wantErrIs      error
		wantErrAs      any
		check          func(t *testing.T, got model.Room)
		wantHTTPStatus int
	}

	cases := []testCase{
		{
			name:  "TC-055 admin creates a valid room",
			actor: testActor(model.RoleAdmin),
			input: RoomCreateInput{Name: "  Aurora  ", Capacity: 8, Floor: 3, Equipment: []model.Equipment{model.EquipmentProjector}},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.createFn = func(_ context.Context, _ model.Room) error { return nil }
			},
			check: func(t *testing.T, got model.Room) {
				assert.True(t, strings.HasPrefix(got.ID, "r-"), "generated id has r- prefix")
				assert.Equal(t, "Aurora", got.Name, "name trimmed")
				assert.Equal(t, 8, got.Capacity)
				assert.Equal(t, 3, got.Floor)
				assert.Equal(t, model.RoomStatusActive, got.Status)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:           "TC-056 non-admin cannot create a room",
			actor:          testActor(model.RoleManager),
			input:          RoomCreateInput{Name: "Aurora", Capacity: 8, Floor: 2},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrIs:      ErrForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:           "TC-057 empty name rejected",
			actor:          testActor(model.RoleAdmin),
			input:          RoomCreateInput{Name: "   ", Capacity: 8, Floor: 2},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-058 capacity below 1 rejected",
			actor:          testActor(model.RoleAdmin),
			input:          RoomCreateInput{Name: "Aurora", Capacity: 0, Floor: 2},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-059 unknown equipment rejected",
			actor:          testActor(model.RoleAdmin),
			input:          RoomCreateInput{Name: "Aurora", Capacity: 8, Floor: 2, Equipment: []model.Equipment{"hologram"}},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-060 duplicate equipment is de-duplicated",
			actor: testActor(model.RoleAdmin),
			input: RoomCreateInput{
				Name: "Aurora", Capacity: 8, Floor: 2,
				Equipment: []model.Equipment{model.EquipmentProjector, model.EquipmentProjector, model.EquipmentWhiteboard},
			},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.createFn = func(_ context.Context, _ model.Room) error { return nil }
			},
			check: func(t *testing.T, got model.Room) {
				assert.Equal(t, []model.Equipment{model.EquipmentProjector, model.EquipmentWhiteboard}, got.Equipment)
			},
			wantHTTPStatus: http.StatusCreated,
		},
		{
			name:  "TC-061 repository failure on create is propagated",
			actor: testActor(model.RoleAdmin),
			input: RoomCreateInput{Name: "Aurora", Capacity: 8, Floor: 2},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.createFn = func(_ context.Context, _ model.Room) error { return errAny }
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rooms := &mockRoomRepo{}
			tc.setupMocks(rooms)

			svc := newTestRoomService(rooms, &mockBookingsForRoom{})
			got, err := svc.Create(context.Background(), tc.actor, tc.input)

			switch {
			case tc.wantErrIs != nil:
				assert.ErrorIs(t, err, tc.wantErrIs)
				assert.Empty(t, got.ID)
			case tc.wantErrAs != nil:
				assert.Error(t, err)
				assert.True(t, errors.As(err, tc.wantErrAs),
					"expected typed error %T, got %T (%v)", tc.wantErrAs, err, err)
				assert.Empty(t, got.ID)
			default:
				assert.NoError(t, err)
				assert.NotEmpty(t, got.ID)
				if tc.check != nil {
					tc.check(t, got)
				}
			}
		})
	}
}

// --- Update --------------------------------------------------------------

func TestRoomService_Update(t *testing.T) {
	existing := model.Room{
		ID: testRoomID, Name: "Old", Capacity: 4, Floor: 2,
		Status: model.RoomStatusActive, Equipment: []model.Equipment{model.EquipmentProjector},
	}
	name := "New Name"
	cap5 := 5
	cap0 := 0
	floor7 := 7
	equipDup := []model.Equipment{model.EquipmentWhiteboard, model.EquipmentWhiteboard}

	type testCase struct {
		name           string
		actor          Actor
		input          RoomUpdateInput
		setupMocks     func(rooms *mockRoomRepo)
		wantErrIs      error
		wantErrAs      any
		check          func(t *testing.T, got model.Room)
		wantHTTPStatus int
	}

	cases := []testCase{
		{
			name:  "TC-062 admin updates name/capacity/floor/equipment",
			actor: testActor(model.RoleAdmin),
			input: RoomUpdateInput{Name: &name, Capacity: &cap5, Floor: &floor7, Equipment: &equipDup},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
				rooms.updateFn = func(_ context.Context, _ model.Room) error { return nil }
			},
			check: func(t *testing.T, got model.Room) {
				assert.Equal(t, "New Name", got.Name)
				assert.Equal(t, 5, got.Capacity)
				assert.Equal(t, 7, got.Floor)
				assert.Equal(t, []model.Equipment{model.EquipmentWhiteboard}, got.Equipment, "equipment de-duplicated")
			},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "TC-063 non-admin cannot update",
			actor:          testActor(model.RoleMember),
			input:          RoomUpdateInput{Name: &name},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrIs:      ErrForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:  "TC-064 update non-existent room",
			actor: testActor(model.RoleAdmin),
			input: RoomUpdateInput{Name: &name},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
					return model.Room{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrRoomNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name:  "TC-065 update to invalid capacity rejected",
			actor: testActor(model.RoleAdmin),
			input: RoomUpdateInput{Capacity: &cap0},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
			},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-066 concurrent delete surfaces as room not found on update",
			actor: testActor(model.RoleAdmin),
			input: RoomUpdateInput{Name: &name},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
				rooms.updateFn = func(_ context.Context, _ model.Room) error { return repository.ErrNotFound }
			},
			wantErrIs:      ErrRoomNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name:  "TC-067 repository failure on update is propagated",
			actor: testActor(model.RoleAdmin),
			input: RoomUpdateInput{Name: &name},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
				rooms.updateFn = func(_ context.Context, _ model.Room) error { return errAny }
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rooms := &mockRoomRepo{}
			tc.setupMocks(rooms)

			svc := newTestRoomService(rooms, &mockBookingsForRoom{})
			got, err := svc.Update(context.Background(), tc.actor, testRoomID, tc.input)

			switch {
			case tc.wantErrIs != nil:
				assert.ErrorIs(t, err, tc.wantErrIs)
				assert.Empty(t, got.ID)
			case tc.wantErrAs != nil:
				assert.Error(t, err)
				assert.True(t, errors.As(err, tc.wantErrAs),
					"expected typed error %T, got %T (%v)", tc.wantErrAs, err, err)
				assert.Empty(t, got.ID)
			default:
				assert.NoError(t, err)
				assert.Equal(t, testRoomID, got.ID)
				if tc.check != nil {
					tc.check(t, got)
				}
			}
		})
	}
}

// --- Delete --------------------------------------------------------------

func TestRoomService_Delete(t *testing.T) {
	existing := testRoom(2)

	type testCase struct {
		name           string
		actor          Actor
		setupMocks     func(rooms *mockRoomRepo, bookings *mockBookingsForRoom)
		wantErrIs      error
		wantHTTPStatus int
	}

	cases := []testCase{
		{
			name:  "TC-068 admin deletes a room without active bookings",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(rooms *mockRoomRepo, bookings *mockBookingsForRoom) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
				bookings.hasActiveFn = func(_ context.Context, _ string, _ time.Time) (bool, error) { return false, nil }
				rooms.deleteFn = func(_ context.Context, _ string) error { return nil }
			},
			wantHTTPStatus: http.StatusNoContent,
		},
		{
			name:           "TC-069 non-admin cannot delete",
			actor:          testActor(model.RoleManager),
			setupMocks:     func(*mockRoomRepo, *mockBookingsForRoom) {},
			wantErrIs:      ErrForbidden,
			wantHTTPStatus: http.StatusForbidden,
		},
		{
			name:  "TC-070 delete non-existent room",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(rooms *mockRoomRepo, _ *mockBookingsForRoom) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
					return model.Room{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrRoomNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name:  "TC-071 delete blocked by active bookings",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(rooms *mockRoomRepo, bookings *mockBookingsForRoom) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
				bookings.hasActiveFn = func(_ context.Context, _ string, _ time.Time) (bool, error) { return true, nil }
			},
			wantErrIs:      ErrRoomHasActiveBookings,
			wantHTTPStatus: http.StatusConflict,
		},
		{
			name:  "TC-072 active-bookings check failure is propagated",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(rooms *mockRoomRepo, bookings *mockBookingsForRoom) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
				bookings.hasActiveFn = func(_ context.Context, _ string, _ time.Time) (bool, error) { return false, errAny }
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
		{
			name:  "TC-073 concurrent delete surfaces as room not found",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(rooms *mockRoomRepo, bookings *mockBookingsForRoom) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) { return existing, nil }
				bookings.hasActiveFn = func(_ context.Context, _ string, _ time.Time) (bool, error) { return false, nil }
				rooms.deleteFn = func(_ context.Context, _ string) error { return repository.ErrNotFound }
			},
			wantErrIs:      ErrRoomNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rooms := &mockRoomRepo{}
			bookings := &mockBookingsForRoom{}
			tc.setupMocks(rooms, bookings)

			svc := newTestRoomService(rooms, bookings)
			err := svc.Delete(context.Background(), tc.actor, testRoomID)

			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// --- Available -----------------------------------------------------------

func TestRoomService_Available(t *testing.T) {
	cap2 := 2
	cap0 := 0
	floor := 2

	type testCase struct {
		name           string
		query          AvailableQuery
		setupMocks     func(rooms *mockRoomRepo)
		wantErrIs      error
		wantErrAs      any
		wantLen        int
		check          func(t *testing.T, gotStart, gotEnd string, gotEq []string)
		wantHTTPStatus int
	}

	cases := []testCase{
		{
			name:  "TC-074 available rooms returned; times normalized to UTC, equipment forwarded",
			query: AvailableQuery{Start: baseStart, End: baseEnd, CapacityMin: &cap2, Floor: &floor, Equipment: []model.Equipment{model.EquipmentProjector}},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.availableFn = func(_ context.Context, _, _ string, _ *int, _ *int, _ []string) ([]model.Room, error) {
					return []model.Room{testRoom(2)}, nil
				}
			},
			wantLen: 1,
			check: func(t *testing.T, gotStart, gotEnd string, gotEq []string) {
				assert.Equal(t, baseStart.UTC().Format(time.RFC3339), gotStart)
				assert.Equal(t, baseEnd.UTC().Format(time.RFC3339), gotEnd)
				assert.Equal(t, []string{"projector"}, gotEq)
			},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name:           "TC-075 end not after start rejected",
			query:          AvailableQuery{Start: baseEnd, End: baseStart},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrIs:      ErrInvalidTimeRange,
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-076 capacity_min below 1 rejected",
			query:          AvailableQuery{Start: baseStart, End: baseEnd, CapacityMin: &cap0},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:           "TC-077 unknown equipment rejected",
			query:          AvailableQuery{Start: baseStart, End: baseEnd, Equipment: []model.Equipment{"hologram"}},
			setupMocks:     func(*mockRoomRepo) {},
			wantErrAs:      new(*ValidationError),
			wantHTTPStatus: http.StatusBadRequest,
		},
		{
			name:  "TC-078 repository failure is propagated",
			query: AvailableQuery{Start: baseStart, End: baseEnd},
			setupMocks: func(rooms *mockRoomRepo) {
				rooms.availableFn = func(_ context.Context, _, _ string, _ *int, _ *int, _ []string) ([]model.Room, error) {
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
			rooms := &mockRoomRepo{}
			tc.setupMocks(rooms)

			// Захватываем аргументы, переданные в репозиторий.
			var gotStart, gotEnd string
			var gotEq []string
			if orig := rooms.availableFn; orig != nil {
				rooms.availableFn = func(ctx context.Context, start, end string, cm *int, fl *int, eq []string) ([]model.Room, error) {
					gotStart, gotEnd, gotEq = start, end, eq
					return orig(ctx, start, end, cm, fl, eq)
				}
			}

			svc := newTestRoomService(rooms, &mockBookingsForRoom{})
			got, err := svc.Available(context.Background(), testActor(model.RoleMember), tc.query)

			switch {
			case tc.wantErrIs != nil:
				assert.ErrorIs(t, err, tc.wantErrIs)
				assert.Nil(t, got)
			case tc.wantErrAs != nil:
				assert.Error(t, err)
				assert.True(t, errors.As(err, tc.wantErrAs),
					"expected typed error %T, got %T (%v)", tc.wantErrAs, err, err)
				assert.Nil(t, got)
			default:
				assert.NoError(t, err)
				assert.Len(t, got, tc.wantLen)
				if tc.check != nil {
					tc.check(t, gotStart, gotEnd, gotEq)
				}
			}
		})
	}
}

// --- List / Get / BookingsOnDate ----------------------------------------

func TestRoomService_Reads(t *testing.T) {
	t.Run("TC-079 List forwards filter and returns rooms with total", func(t *testing.T) {
		rooms := &mockRoomRepo{
			listFn: func(_ context.Context, f repository.RoomFilter) ([]model.Room, int, error) {
				assert.Equal(t, 10, f.Limit)
				return []model.Room{testRoom(2), testRoom(3)}, 2, nil
			},
		}
		svc := newTestRoomService(rooms, &mockBookingsForRoom{})
		got, total, err := svc.List(context.Background(), Actor{}, repository.RoomFilter{Limit: 10})
		assert.NoError(t, err)
		assert.Len(t, got, 2)
		assert.Equal(t, 2, total)
	})

	t.Run("TC-080 Get returns an existing room", func(t *testing.T) {
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return testRoom(2), nil },
		}
		svc := newTestRoomService(rooms, &mockBookingsForRoom{})
		got, err := svc.Get(context.Background(), Actor{}, testRoomID)
		assert.NoError(t, err)
		assert.Equal(t, testRoomID, got.ID)
	})

	t.Run("TC-081 Get maps repository not-found to ErrRoomNotFound", func(t *testing.T) {
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return model.Room{}, repository.ErrNotFound },
		}
		svc := newTestRoomService(rooms, &mockBookingsForRoom{})
		_, err := svc.Get(context.Background(), Actor{}, "ghost")
		assert.ErrorIs(t, err, ErrRoomNotFound)
	})

	t.Run("TC-082 Get propagates an unexpected repository error", func(t *testing.T) {
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return model.Room{}, errAny },
		}
		svc := newTestRoomService(rooms, &mockBookingsForRoom{})
		_, err := svc.Get(context.Background(), Actor{}, testRoomID)
		assert.ErrorIs(t, err, errAny)
	})

	t.Run("TC-083 BookingsOnDate returns bookings for an existing room", func(t *testing.T) {
		date := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return testRoom(2), nil },
		}
		bookings := &mockBookingsForRoom{
			listByRoomOnDateFn: func(_ context.Context, _ string, _ time.Time) ([]model.Booking, error) {
				return []model.Booking{testBooking(t, testUserID, baseStart, model.StatusConfirmed)}, nil
			},
		}
		svc := newTestRoomService(rooms, bookings)
		got, err := svc.BookingsOnDate(context.Background(), Actor{}, testRoomID, date)
		assert.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("TC-084 BookingsOnDate returns ErrRoomNotFound for a missing room", func(t *testing.T) {
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return model.Room{}, repository.ErrNotFound },
		}
		svc := newTestRoomService(rooms, &mockBookingsForRoom{})
		_, err := svc.BookingsOnDate(context.Background(), Actor{}, "ghost", fixedNow)
		assert.ErrorIs(t, err, ErrRoomNotFound)
	})
}

// --- Stats ---------------------------------------------------------------

func TestRoomService_Stats(t *testing.T) {
	t.Run("TC-085 Stats returns count for the last month with the computed window", func(t *testing.T) {
		var gotFrom, gotTo time.Time
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return testRoom(2), nil },
		}
		bookings := &mockBookingsForRoom{
			countInPeriodFn: func(_ context.Context, _ string, from, to time.Time) (int, error) {
				gotFrom, gotTo = from, to
				return 7, nil
			},
		}
		svc := newTestRoomService(rooms, bookings)

		got, err := svc.Stats(context.Background(), testActor(model.RoleMember), testRoomID)
		assert.NoError(t, err)
		assert.Equal(t, testRoomID, got.RoomID)
		assert.Equal(t, 7, got.BookingCount)
		assert.True(t, got.PeriodEnd.Equal(fixedNow), "период заканчивается в now")
		assert.True(t, got.PeriodStart.Equal(fixedNow.AddDate(0, -1, 0)), "период начинается за месяц до now")
		assert.True(t, gotFrom.Equal(fixedNow.AddDate(0, -1, 0)), "в репозиторий передан from = now-1мес")
		assert.True(t, gotTo.Equal(fixedNow), "в репозиторий передан to = now")
	})

	t.Run("TC-086 Stats returns ErrRoomNotFound for a missing room", func(t *testing.T) {
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return model.Room{}, repository.ErrNotFound },
		}
		svc := newTestRoomService(rooms, &mockBookingsForRoom{})
		_, err := svc.Stats(context.Background(), testActor(model.RoleMember), "ghost")
		assert.ErrorIs(t, err, ErrRoomNotFound)
	})

	t.Run("TC-087 Stats propagates an unexpected repository error", func(t *testing.T) {
		rooms := &mockRoomRepo{
			getFn: func(_ context.Context, _ string) (model.Room, error) { return testRoom(2), nil },
		}
		bookings := &mockBookingsForRoom{
			countInPeriodFn: func(_ context.Context, _ string, _, _ time.Time) (int, error) { return 0, errAny },
		}
		svc := newTestRoomService(rooms, bookings)
		_, err := svc.Stats(context.Background(), testActor(model.RoleMember), testRoomID)
		assert.ErrorIs(t, err, errAny)
	})
}
