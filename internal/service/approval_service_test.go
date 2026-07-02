package service

// Юнит-тесты workflow одобрения броней больших переговорок
// (OpenSpec change add-large-room-approval, спека specs/large-room-approval).
//
// ВНИМАНИЕ: это TDD-тесты — они описывают ещё не реализованный контракт и НЕ
// компилируются, пока не добавлены символы фичи. Ожидаемая поверхность (по
// design.md изменения):
//
//   model:   StatusPendingApproval / StatusApproved / StatusRejected;
//            поля Booking.RejectionReason *string и Booking.CreatedAt time.Time
//            (CreatedAt — якорь 24-часового таймаута, заполняется репозиторием).
//   events:  TypeBookingPendingApproval / TypeBookingApproved / TypeBookingRejected.
//   errors:  ErrApprovalNotFound (404), ErrNotPendingApproval (409).
//   service: константы LargeRoomCapacityThreshold = 12, ApprovalTimeout = 24h;
//            Booking.Create ветвится по room.Capacity;
//            Booking.Approve(ctx, Actor, id) (model.Booking, error);
//            Booking.Reject(ctx, Actor, id, reason) (model.Booking, error).
//   repo:    BookingRepo расширяется методами
//            Approve(ctx, id, now) (model.Booking, error) — условный перевод
//              pending_approval → approved; ErrNotFound, если бронь уже не pending;
//            RejectAndOfferWaitlist(ctx, id, reason, now)
//              (model.Booking, *model.WaitlistEntry, error) — атомарный reject с
//              освобождением слота и предложением его листу ожидания (аналог
//              CancelAndOfferWaitlist). ErrNotFound, если бронь уже не pending.
//
// Имена методов сервиса выбраны по конвенции пакета (короткие verbNoun: Create,
// Cancel, Approve, Reject — как ForceCancel), а не CreateBookingWithApproval и т.п.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/testutil"
)

// --- Mock methods (поля объявлены в booking_service_test.go) --------------

func (m *mockBookingRepo) Approve(ctx context.Context, id string, now time.Time) (model.Booking, error) {
	if m.approveFn == nil {
		panic("mockBookingRepo.Approve: not set up")
	}
	return m.approveFn(ctx, id, now)
}

func (m *mockBookingRepo) RejectAndOfferWaitlist(ctx context.Context, id, reason string, now time.Time) (model.Booking, *model.WaitlistEntry, error) {
	if m.rejectAndOfferFn == nil {
		panic("mockBookingRepo.RejectAndOfferWaitlist: not set up")
	}
	return m.rejectAndOfferFn(ctx, id, reason, now)
}

// --- Approval fixtures ----------------------------------------------------

const testRejectReason = "комната зарезервирована под мероприятие"

// roomWithCapacity — room lookup возвращает активную комнату заданной
// вместимости (порог одобрения — строго >12 мест).
func roomWithCapacity(rooms *mockRoomLookup, capacity int) {
	rooms.getFn = func(_ context.Context, _ string) (model.Room, error) {
		return testutil.Room(testutil.WithRoomID(testRoomID), testutil.WithCapacity(capacity)), nil
	}
}

// pendingApproval — бронь большой комнаты в статусе pending_approval с заданным
// моментом создания createdAt (якорь 24-часового таймаута). ID закреплён за
// testBookingID: Approve/Reject сверяют его.
func pendingApproval(t *testing.T, createdAt time.Time) model.Booking {
	t.Helper()
	b := testutil.NewBookingBuilder(t).
		WithRoom(testutil.Room(testutil.WithRoomID(testRoomID), testutil.WithCapacity(20))).
		WithUser(testutil.User(testutil.WithUserID(testUserID))).
		WithTime(baseStart, baseEnd).
		WithStatus(string(model.StatusPendingApproval)).
		Build()
	b.ID = testBookingID
	b.CreatedAt = createdAt
	return b
}

// approvedTo — repo.Approve успешно переводит бронь в approved (эхо той брони,
// что отдаёт getFn, со сменой статуса — как реальный репозиторий через RETURNING).
func approvedTo(repo *mockBookingRepo) {
	repo.approveFn = func(ctx context.Context, id string, _ time.Time) (model.Booking, error) {
		b, err := repo.getFn(ctx, id)
		if err != nil {
			return model.Booking{}, err
		}
		b.Status = model.StatusApproved
		return b, nil
	}
}

// rejectedWithReason — repo.RejectAndOfferWaitlist переводит бронь в rejected,
// сохраняет причину и никого не предлагает из очереди (offered == nil).
func rejectedWithReason(repo *mockBookingRepo) {
	repo.rejectAndOfferFn = func(ctx context.Context, id, reason string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
		b, err := repo.getFn(ctx, id)
		if err != nil {
			return model.Booking{}, nil, err
		}
		b.Status = model.StatusRejected
		b.RejectionReason = &reason
		return b, nil, nil
	}
}

// --- Create: ветка одобрения больших комнат ------------------------------

// TestBookingService_CreateWithApproval покрывает ветвление Create по
// вместимости: >12 мест → pending_approval (HTTP 202) + событие
// booking.pending_approval; ≤12 → прежнее поведение confirmed (HTTP 201) +
// booking.created; занятый слот большой комнаты по-прежнему даёт конфликт
// (pending_approval блокирует слот — это проверяется на уровне репозитория,
// здесь эмулируется конфликтом при вставке).
func TestBookingService_CreateWithApproval(t *testing.T) {
	type testCase struct {
		name          string
		capacity      int
		setupRepo     func(repo *mockBookingRepo)
		wantStatus    model.BookingStatus
		wantErrAs     any
		wantEventType string
		wantHTTPCode  int // справочно
	}

	cases := []testCase{
		{
			name:          "large room (cap 20) -> pending_approval, 202",
			capacity:      20,
			setupRepo:     noConflictInsert,
			wantStatus:    model.StatusPendingApproval,
			wantEventType: events.TypeBookingPendingApproval,
			wantHTTPCode:  http.StatusAccepted,
		},
		{
			name:          "capacity 13 (just above threshold) -> pending_approval",
			capacity:      13,
			setupRepo:     noConflictInsert,
			wantStatus:    model.StatusPendingApproval,
			wantEventType: events.TypeBookingPendingApproval,
			wantHTTPCode:  http.StatusAccepted,
		},
		{
			name:          "capacity 12 (threshold boundary) -> confirmed, 201",
			capacity:      12,
			setupRepo:     noConflictInsert,
			wantStatus:    model.StatusConfirmed,
			wantEventType: events.TypeBookingCreated,
			wantHTTPCode:  http.StatusCreated,
		},
		{
			name:          "small room (cap 10) -> confirmed, 201",
			capacity:      10,
			setupRepo:     noConflictInsert,
			wantStatus:    model.StatusConfirmed,
			wantEventType: events.TypeBookingCreated,
			wantHTTPCode:  http.StatusCreated,
		},
		{
			name:     "large room but slot busy -> conflict (pending_approval holds the slot)",
			capacity: 20,
			setupRepo: func(repo *mockBookingRepo) {
				repo.createCheckedFn = conflictAt(t, baseStart, baseEnd)
			},
			wantErrAs:    new(*BookingConflictError),
			wantHTTPCode: http.StatusConflict,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rooms := &mockRoomLookup{}
			repo := &mockBookingRepo{}
			roomWithCapacity(rooms, tc.capacity)
			tc.setupRepo(repo)

			pub := &mockPublisher{}
			svc := newTestServiceWithPublisher(rooms, repo, pub)

			input := BookingCreateInput{
				RoomID:    testRoomID,
				Title:     "Планёрка",
				StartTime: baseStart,
				EndTime:   baseEnd,
			}
			got, err := svc.Create(context.Background(), testActor(model.RoleMember), input)

			if tc.wantErrAs != nil {
				assert.Error(t, err)
				assert.True(t, errors.As(err, tc.wantErrAs),
					"expected typed error %T, got %T (%v)", tc.wantErrAs, err, err)
				assert.Empty(t, got.ID)
				assert.Empty(t, pub.calls(), "no event on failed create")
				return
			}

			assert.NoError(t, err)
			assert.NotEmpty(t, got.ID, "expected booking id to be assigned")
			assert.Equal(t, testRoomID, got.RoomID)
			assert.Equal(t, testUserID, got.UserID)
			assert.Equal(t, tc.wantStatus, got.Status)

			calls := pub.calls()
			assert.Len(t, calls, 1, "exactly one event published")
			assert.Equal(t, tc.wantEventType, calls[0].event.Type)
			assert.Equal(t, got.ID, calls[0].event.BookingID)
		})
	}
}

// --- Approve --------------------------------------------------------------

// TestBookingService_Approve покрывает одобрение брони админом: успешный
// перевод pending_approval → approved с событием booking.approved; отказ
// не-админу (ErrForbidden); идемпотентность по статусу (уже approved/rejected/
// cancelled → ErrNotPendingApproval); отсутствие брони; ленивый 24-часовой
// таймаут (просрочка → авто-reject + событие booking.rejected, одобрение
// невозможно); гонка/идемпотентность на уровне репозитория; проброс ошибок.
func TestBookingService_Approve(t *testing.T) {
	type testCase struct {
		name          string
		actor         Actor
		setupMocks    func(repo *mockBookingRepo)
		wantStatus    model.BookingStatus // для успешного одобрения
		wantErrIs     error
		wantEventType string // ожидаемый тип единственного события ("" — событий нет)
		wantHTTPCode  int    // справочно
	}

	freshCreatedAt := fixedNow.Add(-time.Hour) // 1 час назад — в пределах таймаута

	cases := []testCase{
		{
			name:  "admin approves pending -> approved + event",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, freshCreatedAt))
				approvedTo(repo)
			},
			wantStatus:    model.StatusApproved,
			wantEventType: events.TypeBookingApproved,
			wantHTTPCode:  http.StatusOK,
		},
		{
			name:  "exactly 24h old (boundary) still approvable",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, fixedNow.Add(-ApprovalTimeout)))
				approvedTo(repo)
			},
			wantStatus:    model.StatusApproved,
			wantEventType: events.TypeBookingApproved,
			wantHTTPCode:  http.StatusOK,
		},
		{
			name:  "member cannot approve",
			actor: testActor(model.RoleMember),
			// Репозиторий не трогается: проверка прав идёт до похода в БД.
			setupMocks:   func(*mockBookingRepo) {},
			wantErrIs:    ErrForbidden,
			wantHTTPCode: http.StatusForbidden,
		},
		{
			name:         "manager cannot approve",
			actor:        testActor(model.RoleManager),
			setupMocks:   func(*mockBookingRepo) {},
			wantErrIs:    ErrForbidden,
			wantHTTPCode: http.StatusForbidden,
		},
		{
			name:  "already approved -> not pending (idempotency)",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				b := pendingApproval(t, freshCreatedAt)
				b.Status = model.StatusApproved
				bookingGet(repo, b)
			},
			wantErrIs:    ErrNotPendingApproval,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:  "already rejected -> not pending (idempotency)",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				b := pendingApproval(t, freshCreatedAt)
				b.Status = model.StatusRejected
				bookingGet(repo, b)
			},
			wantErrIs:    ErrNotPendingApproval,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:  "cancelled by user -> not pending (cancel wins over approval)",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				b := pendingApproval(t, freshCreatedAt)
				b.Status = model.StatusCancelled
				bookingGet(repo, b)
			},
			wantErrIs:    ErrNotPendingApproval,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:  "not found -> approval not found",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
					return model.Booking{}, repository.ErrNotFound
				}
			},
			wantErrIs:    ErrApprovalNotFound,
			wantHTTPCode: http.StatusNotFound,
		},
		{
			name:  "timed out (>24h) -> auto-rejected, approval fails",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, fixedNow.Add(-ApprovalTimeout-time.Second)))
				rejectedWithReason(repo) // авто-reject по таймауту
			},
			wantErrIs:     ErrNotPendingApproval,
			wantEventType: events.TypeBookingRejected, // авто-reject публикует booking.rejected
			wantHTTPCode:  http.StatusConflict,
		},
		{
			name:  "race: repo reports no longer pending -> not pending",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, freshCreatedAt))
				repo.approveFn = func(_ context.Context, _ string, _ time.Time) (model.Booking, error) {
					return model.Booking{}, repository.ErrNotFound
				}
			},
			wantErrIs:    ErrNotPendingApproval,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:  "repository Get failure is propagated",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
					return model.Booking{}, errAny
				}
			},
			wantErrIs:    errAny,
			wantHTTPCode: http.StatusInternalServerError,
		},
		{
			name:  "repository Approve failure is propagated",
			actor: testActor(model.RoleAdmin),
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, freshCreatedAt))
				repo.approveFn = func(_ context.Context, _ string, _ time.Time) (model.Booking, error) {
					return model.Booking{}, errAny
				}
			},
			wantErrIs:    errAny,
			wantHTTPCode: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockBookingRepo{}
			tc.setupMocks(repo)

			pub := &mockPublisher{}
			svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, pub)

			got, err := svc.Approve(context.Background(), tc.actor, testBookingID)

			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs, "expected sentinel error")
				assert.Equal(t, model.Booking{}, got, "no booking returned on error")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, testBookingID, got.ID)
				assert.Equal(t, tc.wantStatus, got.Status, "booking marked approved")
			}

			calls := pub.calls()
			if tc.wantEventType == "" {
				assert.Empty(t, calls, "no event expected")
				return
			}
			assert.Len(t, calls, 1, "exactly one event published")
			assert.Equal(t, tc.wantEventType, calls[0].event.Type)
			assert.Equal(t, testBookingID, calls[0].event.BookingID)
		})
	}
}

// --- Reject ---------------------------------------------------------------

// TestBookingService_Reject покрывает отклонение брони админом: перевод
// pending_approval → rejected с сохранением причины и событием booking.rejected;
// обязательность непустой причины (ValidationError); отказ не-админу; повторное
// отклонение уже approved/rejected брони (ErrNotPendingApproval); отсутствие
// брони; гонка; проброс ошибок. Освобождение слота и предложение его листу
// ожидания проверяются отдельными тестами ниже.
func TestBookingService_Reject(t *testing.T) {
	type testCase struct {
		name          string
		actor         Actor
		reason        string
		setupMocks    func(repo *mockBookingRepo)
		wantErrIs     error
		wantErrAs     any
		wantEventType string
		wantHTTPCode  int // справочно
	}

	freshCreatedAt := fixedNow.Add(-time.Hour)

	cases := []testCase{
		{
			name:   "admin rejects pending with reason -> rejected + event",
			actor:  testActor(model.RoleAdmin),
			reason: testRejectReason,
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, freshCreatedAt))
				rejectedWithReason(repo)
			},
			wantEventType: events.TypeBookingRejected,
			wantHTTPCode:  http.StatusOK,
		},
		{
			name:         "empty reason rejected",
			actor:        testActor(model.RoleAdmin),
			reason:       "",
			setupMocks:   func(*mockBookingRepo) {}, // валидация до похода в БД
			wantErrAs:    new(*ValidationError),
			wantHTTPCode: http.StatusBadRequest,
		},
		{
			name:         "whitespace-only reason rejected",
			actor:        testActor(model.RoleAdmin),
			reason:       "   ",
			setupMocks:   func(*mockBookingRepo) {},
			wantErrAs:    new(*ValidationError),
			wantHTTPCode: http.StatusBadRequest,
		},
		{
			name:         "member cannot reject",
			actor:        testActor(model.RoleMember),
			reason:       testRejectReason,
			setupMocks:   func(*mockBookingRepo) {},
			wantErrIs:    ErrForbidden,
			wantHTTPCode: http.StatusForbidden,
		},
		{
			name:   "already rejected -> not pending (idempotency)",
			actor:  testActor(model.RoleAdmin),
			reason: testRejectReason,
			setupMocks: func(repo *mockBookingRepo) {
				b := pendingApproval(t, freshCreatedAt)
				b.Status = model.StatusRejected
				bookingGet(repo, b)
			},
			wantErrIs:    ErrNotPendingApproval,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:   "already approved -> not pending (idempotency)",
			actor:  testActor(model.RoleAdmin),
			reason: testRejectReason,
			setupMocks: func(repo *mockBookingRepo) {
				b := pendingApproval(t, freshCreatedAt)
				b.Status = model.StatusApproved
				bookingGet(repo, b)
			},
			wantErrIs:    ErrNotPendingApproval,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:   "not found -> approval not found",
			actor:  testActor(model.RoleAdmin),
			reason: testRejectReason,
			setupMocks: func(repo *mockBookingRepo) {
				repo.getFn = func(_ context.Context, _ string) (model.Booking, error) {
					return model.Booking{}, repository.ErrNotFound
				}
			},
			wantErrIs:    ErrApprovalNotFound,
			wantHTTPCode: http.StatusNotFound,
		},
		{
			name:   "race: repo reports no longer pending -> not pending",
			actor:  testActor(model.RoleAdmin),
			reason: testRejectReason,
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, freshCreatedAt))
				repo.rejectAndOfferFn = func(_ context.Context, _, _ string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
					return model.Booking{}, nil, repository.ErrNotFound
				}
			},
			wantErrIs:    ErrNotPendingApproval,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:   "repository Reject failure is propagated",
			actor:  testActor(model.RoleAdmin),
			reason: testRejectReason,
			setupMocks: func(repo *mockBookingRepo) {
				bookingGet(repo, pendingApproval(t, freshCreatedAt))
				repo.rejectAndOfferFn = func(_ context.Context, _, _ string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
					return model.Booking{}, nil, errAny
				}
			},
			wantErrIs:    errAny,
			wantHTTPCode: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockBookingRepo{}
			tc.setupMocks(repo)

			pub := &mockPublisher{}
			svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, pub)

			got, err := svc.Reject(context.Background(), tc.actor, testBookingID, tc.reason)

			switch {
			case tc.wantErrIs != nil:
				assert.ErrorIs(t, err, tc.wantErrIs, "expected sentinel error")
				assert.Equal(t, model.Booking{}, got, "no booking returned on error")
			case tc.wantErrAs != nil:
				assert.Error(t, err)
				assert.True(t, errors.As(err, tc.wantErrAs),
					"expected typed error %T, got %T (%v)", tc.wantErrAs, err, err)
				assert.Equal(t, model.Booking{}, got, "no booking returned on error")
			default:
				assert.NoError(t, err)
				assert.Equal(t, testBookingID, got.ID)
				assert.Equal(t, model.StatusRejected, got.Status, "booking marked rejected")
				if assert.NotNil(t, got.RejectionReason, "rejection reason saved") {
					assert.Equal(t, tc.reason, *got.RejectionReason)
				}
			}

			calls := pub.calls()
			if tc.wantEventType == "" {
				assert.Empty(t, calls, "no event expected")
				return
			}
			assert.Len(t, calls, 1, "exactly one event published")
			assert.Equal(t, tc.wantEventType, calls[0].event.Type)
			assert.Equal(t, testBookingID, calls[0].event.BookingID)
		})
	}
}

// TestBookingService_Reject_OffersFreedSlotToWaitlist: отклонение освобождает
// слот, и, если на него есть запись в листе ожидания, слот предлагается ей —
// как при отмене брони (см. booking-waitlist: "Предложение слота при отмене
// брони"). Атомарность reject + предложения слота обеспечивает репозиторий
// (RejectAndOfferWaitlist в одной транзакции), поэтому на уровне сервиса
// проверяем маршрутизацию reject через этот метод и корректную передачу причины.
func TestBookingService_Reject_OffersFreedSlotToWaitlist(t *testing.T) {
	repo := &mockBookingRepo{}
	bookingGet(repo, pendingApproval(t, fixedNow.Add(-time.Hour)))

	var gotReason string
	var called bool
	offered := &model.WaitlistEntry{
		ID:       "wl-1",
		RoomID:   testRoomID,
		UserID:   testOtherID,
		Position: 1,
		Status:   model.WaitlistStatusOffered,
	}
	repo.rejectAndOfferFn = func(ctx context.Context, id, reason string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
		called = true
		gotReason = reason
		b, err := repo.getFn(ctx, id)
		if err != nil {
			return model.Booking{}, nil, err
		}
		b.Status = model.StatusRejected
		b.RejectionReason = &reason
		return b, offered, nil // слот освобождён и предложен первому в очереди
	}

	svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, &mockPublisher{})

	got, err := svc.Reject(context.Background(), testActor(model.RoleAdmin), testBookingID, testRejectReason)
	assert.NoError(t, err)
	assert.True(t, called, "reject must route through RejectAndOfferWaitlist to free+offer the slot atomically")
	assert.Equal(t, testRejectReason, gotReason, "reason forwarded to repository")
	assert.Equal(t, model.StatusRejected, got.Status)
}

// TestBookingService_Reject_NoWaitlistStillSucceeds: если очереди на слот нет,
// reject всё равно проходит успешно (предлагать некому — offered == nil).
func TestBookingService_Reject_NoWaitlistStillSucceeds(t *testing.T) {
	repo := &mockBookingRepo{}
	bookingGet(repo, pendingApproval(t, fixedNow.Add(-time.Hour)))
	rejectedWithReason(repo) // offered == nil

	svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, &mockPublisher{})

	got, err := svc.Reject(context.Background(), testActor(model.RoleAdmin), testBookingID, testRejectReason)
	assert.NoError(t, err)
	assert.Equal(t, model.StatusRejected, got.Status)
}

// TestBookingService_Reject_PublishFailureDoesNotFail: сбой публикации события
// не откатывает отклонение — статус уже зафиксирован в БД (eventual consistency,
// тот же контракт, что у Create/Cancel).
func TestBookingService_Reject_PublishFailureDoesNotFail(t *testing.T) {
	repo := &mockBookingRepo{}
	bookingGet(repo, pendingApproval(t, fixedNow.Add(-time.Hour)))
	rejectedWithReason(repo)

	pub := &mockPublisher{err: errAny}
	svc := newTestServiceWithPublisher(&mockRoomLookup{}, repo, pub)

	got, err := svc.Reject(context.Background(), testActor(model.RoleAdmin), testBookingID, testRejectReason)
	assert.NoError(t, err, "publish failure must not fail the rejection")
	assert.Equal(t, model.StatusRejected, got.Status)
	assert.Len(t, pub.calls(), 1, "publish was attempted")
}
