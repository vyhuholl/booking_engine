package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
)

// --- Mock ----------------------------------------------------------------

type mockWaitlistRepo struct {
	createFn         func(ctx context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error)
	getFn            func(ctx context.Context, id string) (model.WaitlistEntry, error)
	listByRoomFn     func(ctx context.Context, roomID string) ([]model.WaitlistEntry, error)
	deleteAndOfferFn func(ctx context.Context, id string, now time.Time) (*model.WaitlistEntry, error)
	confirmAndBookFn func(ctx context.Context, entryID string, b model.Booking) (*model.Booking, error)
	expireFn         func(ctx context.Context, entryID string, now time.Time) (*model.WaitlistEntry, error)
}

func (m *mockWaitlistRepo) Create(ctx context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error) {
	if m.createFn == nil {
		panic("mockWaitlistRepo.Create: not set up")
	}
	return m.createFn(ctx, e)
}

func (m *mockWaitlistRepo) Get(ctx context.Context, id string) (model.WaitlistEntry, error) {
	if m.getFn == nil {
		panic("mockWaitlistRepo.Get: not set up")
	}
	return m.getFn(ctx, id)
}

func (m *mockWaitlistRepo) ListByRoom(ctx context.Context, roomID string) ([]model.WaitlistEntry, error) {
	if m.listByRoomFn == nil {
		panic("mockWaitlistRepo.ListByRoom: not set up")
	}
	return m.listByRoomFn(ctx, roomID)
}

func (m *mockWaitlistRepo) DeleteAndOfferNext(ctx context.Context, id string, now time.Time) (*model.WaitlistEntry, error) {
	if m.deleteAndOfferFn == nil {
		panic("mockWaitlistRepo.DeleteAndOfferNext: not set up")
	}
	return m.deleteAndOfferFn(ctx, id, now)
}

func (m *mockWaitlistRepo) ConfirmAndBook(ctx context.Context, entryID string, b model.Booking) (*model.Booking, error) {
	if m.confirmAndBookFn == nil {
		panic("mockWaitlistRepo.ConfirmAndBook: not set up")
	}
	return m.confirmAndBookFn(ctx, entryID, b)
}

func (m *mockWaitlistRepo) ExpireAndOfferNext(ctx context.Context, entryID string, now time.Time) (*model.WaitlistEntry, error) {
	if m.expireFn == nil {
		panic("mockWaitlistRepo.ExpireAndOfferNext: not set up")
	}
	return m.expireFn(ctx, entryID, now)
}

// --- Helpers -------------------------------------------------------------

const testWaitlistID = "wl-1"

func newTestWaitlist(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) *Waitlist {
	s := NewWaitlist(rooms, wl, bookings, nil, &mockPublisher{}, testTopic, nil)
	s.now = func() time.Time { return fixedNow }
	return s
}

func waitlistJoinInput() WaitlistJoinInput {
	return WaitlistJoinInput{RoomID: testRoomID, StartTime: baseStart, EndTime: baseEnd}
}

// roomBusy настраивает предикат занятости комнаты.
func roomBusy(repo *mockBookingRepo, busy bool) {
	repo.isRoomBusyFn = func(_ context.Context, _ string, _, _ time.Time) (bool, error) {
		return busy, nil
	}
}

// offeredEntry — offered-запись владельца owner с моментом предложения offeredAt.
func offeredEntry(owner string, offeredAt time.Time) model.WaitlistEntry {
	at := offeredAt
	return model.WaitlistEntry{
		ID:        testWaitlistID,
		RoomID:    testRoomID,
		UserID:    owner,
		StartTime: baseStart,
		EndTime:   baseEnd,
		Position:  1,
		Status:    model.WaitlistStatusOffered,
		OfferedAt: &at,
	}
}

// --- Join tests ----------------------------------------------------------

func TestWaitlistService_Join(t *testing.T) {
	type testCase struct {
		name      string
		setup     func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo)
		input     WaitlistJoinInput
		wantErrIs error
	}

	cases := []testCase{
		{
			name: "success on busy room",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
				roomFound(rooms, 2)
				roomBusy(bookings, true)
				wl.createFn = func(_ context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error) {
					e.Position = 1
					return e, nil
				}
			},
			input: waitlistJoinInput(),
		},
		{
			name: "room available rejects",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
				roomFound(rooms, 2)
				roomBusy(bookings, false)
			},
			input:     waitlistJoinInput(),
			wantErrIs: ErrRoomAvailable,
		},
		{
			name: "duplicate entry",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
				roomFound(rooms, 2)
				roomBusy(bookings, true)
				wl.createFn = func(_ context.Context, _ model.WaitlistEntry) (model.WaitlistEntry, error) {
					return model.WaitlistEntry{}, repository.ErrConflict
				}
			},
			input:     waitlistJoinInput(),
			wantErrIs: ErrAlreadyInWaitlist,
		},
		{
			// Гонка: предпроверка увидела занятость, но к моменту вставки бронь
			// отменили — атомарная проверка внутри Create вернула ErrNoOverlap.
			name: "room freed before insert",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
				roomFound(rooms, 2)
				roomBusy(bookings, true)
				wl.createFn = func(_ context.Context, _ model.WaitlistEntry) (model.WaitlistEntry, error) {
					return model.WaitlistEntry{}, repository.ErrNoOverlap
				}
			},
			input:     waitlistJoinInput(),
			wantErrIs: ErrRoomAvailable,
		},
		{
			name: "start in past",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
				// валидация до похода в репозитории — моки не задаём
			},
			input:     WaitlistJoinInput{RoomID: testRoomID, StartTime: fixedNow.Add(-time.Hour), EndTime: fixedNow},
			wantErrIs: ErrStartInPast,
		},
		{
			name: "duration too short",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
			},
			input:     WaitlistJoinInput{RoomID: testRoomID, StartTime: baseStart, EndTime: baseStart.Add(5 * time.Minute)},
			wantErrIs: ErrDurationTooShort,
		},
		{
			name: "room out of service",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
				roomFoundWithStatus(rooms, 2, model.RoomStatusOutOfService)
			},
			input:     waitlistJoinInput(),
			wantErrIs: ErrRoomOutOfService,
		},
		{
			name: "room not found",
			setup: func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {
				rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
					return model.Room{}, repository.ErrNotFound
				}
			},
			input:     waitlistJoinInput(),
			wantErrIs: ErrRoomNotFound,
		},
		{
			name:      "empty room_id",
			setup:     func(rooms *mockRoomLookup, wl *mockWaitlistRepo, bookings *mockBookingRepo) {},
			input:     WaitlistJoinInput{RoomID: "", StartTime: baseStart, EndTime: baseEnd},
			wantErrIs: nil, // ValidationError, проверяется отдельно ниже
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rooms := &mockRoomLookup{}
			wl := &mockWaitlistRepo{}
			bookings := &mockBookingRepo{}
			tc.setup(rooms, wl, bookings)

			svc := newTestWaitlist(rooms, wl, bookings)
			got, err := svc.Join(context.Background(), testActor(model.RoleMember), tc.input)

			if tc.name == "empty room_id" {
				var verr *ValidationError
				assert.ErrorAs(t, err, &verr)
				return
			}
			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs)
				assert.Equal(t, model.WaitlistEntry{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, model.WaitlistStatusWaiting, got.Status)
			assert.Equal(t, testUserID, got.UserID)
			assert.Equal(t, 1, got.Position)
			assert.NotEmpty(t, got.ID)
		})
	}
}

// --- Confirm tests -------------------------------------------------------

// activeRooms — mockRoomLookup, чей Get всегда возвращает активную комнату. Нужен
// Confirm'у: перед созданием брони он проверяет статус комнаты.
func activeRooms() *mockRoomLookup {
	r := &mockRoomLookup{}
	roomFound(r, 2)
	return r
}

func TestWaitlistService_Confirm_Success(t *testing.T) {
	wl := &mockWaitlistRepo{
		getFn: func(_ context.Context, _ string) (model.WaitlistEntry, error) {
			return offeredEntry(testUserID, fixedNow), nil // предложено «только что»
		},
		confirmAndBookFn: func(_ context.Context, _ string, _ model.Booking) (*model.Booking, error) {
			return nil, nil // нет конфликта
		},
	}
	svc := newTestWaitlist(activeRooms(), wl, &mockBookingRepo{})

	got, err := svc.Confirm(context.Background(), testActor(model.RoleMember), testWaitlistID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusConfirmed, got.Status)
	assert.Equal(t, testRoomID, got.RoomID)
	assert.Equal(t, testUserID, got.UserID)
	assert.NotEmpty(t, got.ID)
}

func TestWaitlistService_Confirm_Errors(t *testing.T) {
	type testCase struct {
		name         string
		setup        func(wl *mockWaitlistRepo)
		actor        Actor
		wantErrIs    error
		wantConflict bool
	}

	cases := []testCase{
		{
			name: "not found",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return model.WaitlistEntry{}, repository.ErrNotFound
				}
			},
			actor:     testActor(model.RoleMember),
			wantErrIs: ErrWaitlistNotFound,
		},
		{
			name: "not owner",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return offeredEntry(testOtherID, fixedNow), nil
				}
			},
			actor:     testActor(model.RoleMember),
			wantErrIs: ErrWaitlistForbidden,
		},
		{
			name: "not offered",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					e := offeredEntry(testUserID, fixedNow)
					e.Status = model.WaitlistStatusWaiting
					e.OfferedAt = nil
					return e, nil
				}
			},
			actor:     testActor(model.RoleMember),
			wantErrIs: ErrOfferNotPending,
		},
		{
			name: "expired offer triggers next offer",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return offeredEntry(testUserID, fixedNow.Add(-OfferTTL-time.Minute)), nil
				}
				wl.expireFn = func(_ context.Context, _ string, _ time.Time) (*model.WaitlistEntry, error) {
					next := offeredEntry(testOtherID, fixedNow)
					next.Position = 2
					return &next, nil
				}
			},
			actor:     testActor(model.RoleMember),
			wantErrIs: ErrOfferExpired,
		},
		{
			name: "booking conflict on confirm",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return offeredEntry(testUserID, fixedNow), nil
				}
				wl.confirmAndBookFn = func(_ context.Context, _ string, _ model.Booking) (*model.Booking, error) {
					c := model.Booking{ID: "b-conflict", RoomID: testRoomID, StartTime: baseStart, EndTime: baseEnd}
					return &c, nil
				}
			},
			actor:        testActor(model.RoleMember),
			wantConflict: true,
		},
		{
			name: "race: entry no longer offered at persist",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return offeredEntry(testUserID, fixedNow), nil
				}
				wl.confirmAndBookFn = func(_ context.Context, _ string, _ model.Booking) (*model.Booking, error) {
					return nil, repository.ErrNotFound
				}
			},
			actor:     testActor(model.RoleMember),
			wantErrIs: ErrOfferNotPending,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			wl := &mockWaitlistRepo{}
			tc.setup(wl)
			svc := newTestWaitlist(activeRooms(), wl, &mockBookingRepo{})

			_, err := svc.Confirm(context.Background(), tc.actor, testWaitlistID)
			if tc.wantConflict {
				var conflict *BookingConflictError
				assert.ErrorAs(t, err, &conflict)
				return
			}
			assert.ErrorIs(t, err, tc.wantErrIs)
		})
	}
}

func TestWaitlistService_Confirm_PublishesEventAndInvalidatesCache(t *testing.T) {
	wl := &mockWaitlistRepo{
		getFn: func(_ context.Context, _ string) (model.WaitlistEntry, error) {
			return offeredEntry(testUserID, fixedNow), nil
		},
		confirmAndBookFn: func(_ context.Context, _ string, _ model.Booking) (*model.Booking, error) {
			return nil, nil
		},
	}
	pub := &mockPublisher{}
	var invalidated []string
	c := &mockRoomCache{invalidateFn: func(_ context.Context, roomID string) error {
		invalidated = append(invalidated, roomID)
		return nil
	}}
	svc := NewWaitlist(activeRooms(), wl, &mockBookingRepo{}, c, pub, testTopic, nil)
	svc.now = func() time.Time { return fixedNow }

	got, err := svc.Confirm(context.Background(), testActor(model.RoleMember), testWaitlistID)
	require.NoError(t, err)

	calls := pub.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, events.TypeBookingCreated, calls[0].event.Type)
	assert.Equal(t, got.ID, calls[0].event.BookingID)
	assert.Equal(t, []string{testRoomID}, invalidated)
}

// --- Leave / ListByRoom tests -------------------------------------------

func TestWaitlistService_Leave(t *testing.T) {
	type testCase struct {
		name      string
		setup     func(wl *mockWaitlistRepo)
		actor     Actor
		wantErrIs error
	}

	cases := []testCase{
		{
			name: "owner leaves waiting entry (nothing re-offered)",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					e := offeredEntry(testUserID, fixedNow)
					e.Status = model.WaitlistStatusWaiting
					e.OfferedAt = nil
					return e, nil
				}
				wl.deleteAndOfferFn = func(_ context.Context, _ string, _ time.Time) (*model.WaitlistEntry, error) {
					return nil, nil
				}
			},
			actor: testActor(model.RoleMember),
		},
		{
			name: "owner leaves offered entry re-offers next",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return offeredEntry(testUserID, fixedNow), nil
				}
				wl.deleteAndOfferFn = func(_ context.Context, _ string, _ time.Time) (*model.WaitlistEntry, error) {
					next := offeredEntry(testOtherID, fixedNow)
					next.Position = 2
					return &next, nil
				}
			},
			actor: testActor(model.RoleMember),
		},
		{
			name: "admin removes another user's entry",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return offeredEntry(testOtherID, fixedNow), nil
				}
				wl.deleteAndOfferFn = func(_ context.Context, _ string, _ time.Time) (*model.WaitlistEntry, error) {
					return nil, nil
				}
			},
			actor: Actor{ID: testAdminID, Role: model.RoleAdmin},
		},
		{
			name: "non-owner forbidden",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return offeredEntry(testOtherID, fixedNow), nil
				}
			},
			actor:     testActor(model.RoleMember),
			wantErrIs: ErrWaitlistForbidden,
		},
		{
			name: "not found",
			setup: func(wl *mockWaitlistRepo) {
				wl.getFn = func(_ context.Context, _ string) (model.WaitlistEntry, error) {
					return model.WaitlistEntry{}, repository.ErrNotFound
				}
			},
			actor:     testActor(model.RoleMember),
			wantErrIs: ErrWaitlistNotFound,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			wl := &mockWaitlistRepo{}
			tc.setup(wl)
			svc := newTestWaitlist(&mockRoomLookup{}, wl, &mockBookingRepo{})

			err := svc.Leave(context.Background(), tc.actor, testWaitlistID)
			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestWaitlistService_ListByRoom(t *testing.T) {
	want := []model.WaitlistEntry{
		{ID: "wl-1", RoomID: testRoomID, Position: 1, Status: model.WaitlistStatusWaiting},
		{ID: "wl-2", RoomID: testRoomID, Position: 2, Status: model.WaitlistStatusWaiting},
	}
	wl := &mockWaitlistRepo{
		listByRoomFn: func(_ context.Context, roomID string) ([]model.WaitlistEntry, error) {
			assert.Equal(t, testRoomID, roomID)
			return want, nil
		},
	}
	svc := newTestWaitlist(&mockRoomLookup{}, wl, &mockBookingRepo{})

	got, err := svc.ListByRoom(context.Background(), testActor(model.RoleMember), testRoomID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestBookingService_Cancel_OffersWaitlistSlot проверяет, что отмена, при которой
// репозиторий предложил слот из очереди, завершается успешно и возвращает
// отменённую бронь (побочный эффект предложения не влияет на результат отмены).
func TestBookingService_Cancel_OffersWaitlistSlot(t *testing.T) {
	repo := &mockBookingRepo{}
	b := testBooking(t, testUserID, baseStart, model.StatusConfirmed)
	bookingGet(repo, b)
	repo.cancelAndOfferFn = func(_ context.Context, _ string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
		cancelled := b
		cancelled.Status = model.StatusCancelled
		offered := offeredEntry(testOtherID, fixedNow)
		return cancelled, &offered, nil
	}

	svc := newTestService(&mockRoomLookup{}, repo)
	got, err := svc.Cancel(context.Background(), testActor(model.RoleMember), testBookingID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusCancelled, got.Status)
}
