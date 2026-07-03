package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
)

// --- Стабы для approval-эндпоинтов ---------------------------------------

// apRepoStub встраивает service.BookingRepo: неопределённые методы паникуют
// (nil-интерфейс), заданные — переопределяются полями-функциями.
type apRepoStub struct {
	service.BookingRepo
	getFn           func(ctx context.Context, id string) (model.Booking, error)
	approveFn       func(ctx context.Context, id string, now time.Time) (model.Booking, error)
	rejectFn        func(ctx context.Context, id, reason string, now time.Time) (model.Booking, *model.WaitlistEntry, error)
	listPendingFn   func(ctx context.Context, now time.Time, timeout time.Duration, reason string) ([]model.Booking, []model.Booking, error)
	createCheckedFn func(ctx context.Context, b model.Booking) (*model.Booking, error)
}

func (s apRepoStub) Get(ctx context.Context, id string) (model.Booking, error) {
	return s.getFn(ctx, id)
}
func (s apRepoStub) Approve(ctx context.Context, id string, now time.Time) (model.Booking, error) {
	return s.approveFn(ctx, id, now)
}
func (s apRepoStub) RejectAndOfferWaitlist(ctx context.Context, id, reason string, now time.Time) (model.Booking, *model.WaitlistEntry, error) {
	return s.rejectFn(ctx, id, reason, now)
}
func (s apRepoStub) ListPendingApprovals(ctx context.Context, now time.Time, timeout time.Duration, reason string) ([]model.Booking, []model.Booking, error) {
	return s.listPendingFn(ctx, now, timeout, reason)
}
func (s apRepoStub) CreateChecked(ctx context.Context, b model.Booking) (*model.Booking, error) {
	return s.createCheckedFn(ctx, b)
}

type apRoomStub struct {
	service.RoomLookup
	getFn func(ctx context.Context, id string) (model.Room, error)
}

func (s apRoomStub) Get(ctx context.Context, id string) (model.Room, error) {
	return s.getFn(ctx, id)
}

func newApprovalHandler(repo service.BookingRepo, rooms service.RoomLookup) *Handler {
	svc := service.NewBooking(rooms, repo, nil, nil, "", slog.Default())
	return New(slog.Default(), "secret", userLookupStub{}, nil, svc, nil, nil)
}

var adminActor = service.Actor{ID: "admin-1", Role: model.RoleAdmin}

func roomOfCapacity(cap int) apRoomStub {
	return apRoomStub{getFn: func(context.Context, string) (model.Room, error) {
		return model.Room{ID: "room-1", Capacity: cap, Status: model.RoomStatusActive}, nil
	}}
}

// --- POST /bookings — 202 vs 201 -----------------------------------------

func TestCreateBooking_LargeRoomReturns202(t *testing.T) {
	repo := apRepoStub{createCheckedFn: func(context.Context, model.Booking) (*model.Booking, error) { return nil, nil }}
	h := newApprovalHandler(repo, roomOfCapacity(20))

	body := `{"room_id":"room-1","title":"Big meeting","start_time":"2027-01-01T10:00:00Z","end_time":"2027-01-01T11:00:00Z"}`
	rec := httptest.NewRecorder()
	h.createBooking(rec, wlRequest(http.MethodPost, "/bookings", "", body, memberActor))

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	var got model.Booking
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, model.StatusPendingApproval, got.Status)
}

func TestCreateBooking_SmallRoomReturns201(t *testing.T) {
	repo := apRepoStub{createCheckedFn: func(context.Context, model.Booking) (*model.Booking, error) { return nil, nil }}
	h := newApprovalHandler(repo, roomOfCapacity(10))

	body := `{"room_id":"room-1","title":"Standup","start_time":"2027-01-01T10:00:00Z","end_time":"2027-01-01T11:00:00Z"}`
	rec := httptest.NewRecorder()
	h.createBooking(rec, wlRequest(http.MethodPost, "/bookings", "", body, memberActor))

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var got model.Booking
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, model.StatusConfirmed, got.Status)
}

// --- GET /admin/approvals -------------------------------------------------

func TestListApprovals_OK(t *testing.T) {
	repo := apRepoStub{listPendingFn: func(context.Context, time.Time, time.Duration, string) ([]model.Booking, []model.Booking, error) {
		return []model.Booking{
			{ID: "b-1", Status: model.StatusPendingApproval},
			{ID: "b-2", Status: model.StatusPendingApproval},
		}, nil, nil
	}}
	h := newApprovalHandler(repo, nil)

	rec := httptest.NewRecorder()
	h.listApprovals(rec, wlRequest(http.MethodGet, "/admin/approvals", "", "", adminActor))

	require.Equal(t, http.StatusOK, rec.Code)
	var got []model.Booking
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
}

func TestListApprovals_ForbiddenForNonAdmin(t *testing.T) {
	// Роль проверяется в сервисе до обращения к репозиторию: стаб без методов
	// (паникует при вызове) подтверждает, что репозиторий не трогается.
	h := newApprovalHandler(apRepoStub{}, nil)

	rec := httptest.NewRecorder()
	h.listApprovals(rec, wlRequest(http.MethodGet, "/admin/approvals", "", "", memberActor))

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "FORBIDDEN", errorCode(t, rec))
}

// --- POST /admin/approvals/{id}/approve ----------------------------------

func TestApproveBooking_OK(t *testing.T) {
	repo := apRepoStub{
		getFn: func(context.Context, string) (model.Booking, error) {
			return model.Booking{ID: "b-1", RoomID: "room-1", Status: model.StatusPendingApproval, CreatedAt: time.Now()}, nil
		},
		approveFn: func(context.Context, string, time.Time) (model.Booking, error) {
			return model.Booking{ID: "b-1", RoomID: "room-1", Status: model.StatusApproved}, nil
		},
	}
	h := newApprovalHandler(repo, nil)

	rec := httptest.NewRecorder()
	h.approveBooking(rec, wlRequest(http.MethodPost, "/admin/approvals/b-1/approve", "b-1", "", adminActor))

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got model.Booking
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, model.StatusApproved, got.Status)
}

func TestApproveBooking_NotFound(t *testing.T) {
	repo := apRepoStub{getFn: func(context.Context, string) (model.Booking, error) {
		return model.Booking{}, repository.ErrNotFound
	}}
	h := newApprovalHandler(repo, nil)

	rec := httptest.NewRecorder()
	h.approveBooking(rec, wlRequest(http.MethodPost, "/admin/approvals/b-x/approve", "b-x", "", adminActor))

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "APPROVAL_NOT_FOUND", errorCode(t, rec))
}

func TestApproveBooking_NotPending(t *testing.T) {
	repo := apRepoStub{getFn: func(context.Context, string) (model.Booking, error) {
		return model.Booking{ID: "b-1", Status: model.StatusApproved, CreatedAt: time.Now()}, nil
	}}
	h := newApprovalHandler(repo, nil)

	rec := httptest.NewRecorder()
	h.approveBooking(rec, wlRequest(http.MethodPost, "/admin/approvals/b-1/approve", "b-1", "", adminActor))

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "NOT_PENDING_APPROVAL", errorCode(t, rec))
}

func TestApproveBooking_ForbiddenForNonAdmin(t *testing.T) {
	h := newApprovalHandler(apRepoStub{}, nil)

	rec := httptest.NewRecorder()
	h.approveBooking(rec, wlRequest(http.MethodPost, "/admin/approvals/b-1/approve", "b-1", "", memberActor))

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "FORBIDDEN", errorCode(t, rec))
}

// --- POST /admin/approvals/{id}/reject -----------------------------------

func TestRejectBooking_OK(t *testing.T) {
	reason := "room reserved for an event"
	repo := apRepoStub{
		getFn: func(context.Context, string) (model.Booking, error) {
			return model.Booking{ID: "b-1", RoomID: "room-1", Status: model.StatusPendingApproval, CreatedAt: time.Now()}, nil
		},
		rejectFn: func(_ context.Context, _, r string, _ time.Time) (model.Booking, *model.WaitlistEntry, error) {
			return model.Booking{ID: "b-1", RoomID: "room-1", Status: model.StatusRejected, RejectionReason: &r}, nil, nil
		},
	}
	h := newApprovalHandler(repo, nil)

	rec := httptest.NewRecorder()
	h.rejectBooking(rec, wlRequest(http.MethodPost, "/admin/approvals/b-1/reject", "b-1", `{"reason":"`+reason+`"}`, adminActor))

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got model.Booking
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, model.StatusRejected, got.Status)
	require.NotNil(t, got.RejectionReason)
	assert.Equal(t, reason, *got.RejectionReason)
}

func TestRejectBooking_EmptyReason(t *testing.T) {
	// reason валидируется в сервисе (после проверки роли) — репозиторий не трогается.
	h := newApprovalHandler(apRepoStub{}, nil)

	rec := httptest.NewRecorder()
	h.rejectBooking(rec, wlRequest(http.MethodPost, "/admin/approvals/b-1/reject", "b-1", `{"reason":"  "}`, adminActor))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "VALIDATION_ERROR", errorCode(t, rec))
}

func TestRejectBooking_InvalidJSON(t *testing.T) {
	h := newApprovalHandler(apRepoStub{}, nil)

	rec := httptest.NewRecorder()
	h.rejectBooking(rec, wlRequest(http.MethodPost, "/admin/approvals/b-1/reject", "b-1", "{not json", adminActor))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "VALIDATION_ERROR", errorCode(t, rec))
}
