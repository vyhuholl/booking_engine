package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
)

// --- Стабы для сервиса Waitlist ------------------------------------------

type wlRepoStub struct {
	service.WaitlistRepo
	createFn         func(ctx context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error)
	getFn            func(ctx context.Context, id string) (model.WaitlistEntry, error)
	listByRoomFn     func(ctx context.Context, roomID string) ([]model.WaitlistEntry, error)
	deleteAndOfferFn func(ctx context.Context, id string, now time.Time) (*model.WaitlistEntry, error)
	confirmAndBookFn func(ctx context.Context, entryID string, b model.Booking) (*model.Booking, error)
}

func (s wlRepoStub) Create(ctx context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error) {
	return s.createFn(ctx, e)
}
func (s wlRepoStub) Get(ctx context.Context, id string) (model.WaitlistEntry, error) {
	return s.getFn(ctx, id)
}
func (s wlRepoStub) ListByRoom(ctx context.Context, roomID string) ([]model.WaitlistEntry, error) {
	return s.listByRoomFn(ctx, roomID)
}
func (s wlRepoStub) DeleteAndOfferNext(ctx context.Context, id string, now time.Time) (*model.WaitlistEntry, error) {
	return s.deleteAndOfferFn(ctx, id, now)
}
func (s wlRepoStub) ConfirmAndBook(ctx context.Context, entryID string, b model.Booking) (*model.Booking, error) {
	return s.confirmAndBookFn(ctx, entryID, b)
}

type wlRoomLookupStub struct {
	service.RoomLookup
	getFn func(ctx context.Context, id string) (model.Room, error)
}

func (s wlRoomLookupStub) Get(ctx context.Context, id string) (model.Room, error) {
	return s.getFn(ctx, id)
}

type wlBookingRepoStub struct {
	service.BookingRepo
	isRoomBusyFn func(ctx context.Context, roomID string, start, end time.Time) (bool, error)
}

func (s wlBookingRepoStub) IsRoomBusy(ctx context.Context, roomID string, start, end time.Time) (bool, error) {
	return s.isRoomBusyFn(ctx, roomID, start, end)
}

func newWaitlistHandler(wl service.WaitlistRepo, rooms service.RoomLookup, bookings service.BookingRepo) *Handler {
	svc := service.NewWaitlist(rooms, wl, bookings, nil, nil, "", slog.Default())
	return New(slog.Default(), "secret", userLookupStub{}, nil, nil, svc, nil)
}

// wlRequest собирает запрос с актором и chi-параметром {id} в контексте (в обход
// роутера и authMiddleware).
func wlRequest(method, target, id, body string, actor service.Actor) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, actorKey, actor)
	return r.WithContext(ctx)
}

var memberActor = service.Actor{ID: "user-1", Role: model.RoleMember}

func activeRoomLookup() wlRoomLookupStub {
	return wlRoomLookupStub{getFn: func(context.Context, string) (model.Room, error) {
		return model.Room{ID: "room-1", Status: model.RoomStatusActive}, nil
	}}
}

// --- join -----------------------------------------------------------------

func TestJoinWaitlist_Created(t *testing.T) {
	wl := wlRepoStub{createFn: func(_ context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error) {
		e.Position = 1
		return e, nil
	}}
	bookings := wlBookingRepoStub{isRoomBusyFn: func(context.Context, string, time.Time, time.Time) (bool, error) {
		return true, nil
	}}
	h := newWaitlistHandler(wl, activeRoomLookup(), bookings)

	body := `{"start_time":"2027-01-01T10:00:00Z","end_time":"2027-01-01T11:00:00Z"}`
	rec := httptest.NewRecorder()
	h.joinWaitlist(rec, wlRequest(http.MethodPost, "/rooms/room-1/waitlist", "room-1", body, memberActor))

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var got model.WaitlistEntry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, model.WaitlistStatusWaiting, got.Status)
	assert.Equal(t, 1, got.Position)
}

func TestJoinWaitlist_RoomAvailableConflict(t *testing.T) {
	bookings := wlBookingRepoStub{isRoomBusyFn: func(context.Context, string, time.Time, time.Time) (bool, error) {
		return false, nil // комната свободна
	}}
	h := newWaitlistHandler(wlRepoStub{}, activeRoomLookup(), bookings)

	body := `{"start_time":"2027-01-01T10:00:00Z","end_time":"2027-01-01T11:00:00Z"}`
	rec := httptest.NewRecorder()
	h.joinWaitlist(rec, wlRequest(http.MethodPost, "/rooms/room-1/waitlist", "room-1", body, memberActor))

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "WAITLIST_ROOM_AVAILABLE", errorCode(t, rec))
}

func TestJoinWaitlist_InvalidJSON(t *testing.T) {
	h := newWaitlistHandler(wlRepoStub{}, activeRoomLookup(), wlBookingRepoStub{})

	rec := httptest.NewRecorder()
	h.joinWaitlist(rec, wlRequest(http.MethodPost, "/rooms/room-1/waitlist", "room-1", "{not json", memberActor))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "VALIDATION_ERROR", errorCode(t, rec))
}

// --- list -----------------------------------------------------------------

func TestListWaitlist_OK(t *testing.T) {
	wl := wlRepoStub{listByRoomFn: func(context.Context, string) ([]model.WaitlistEntry, error) {
		return []model.WaitlistEntry{
			{ID: "wl-1", Position: 1, Status: model.WaitlistStatusWaiting},
			{ID: "wl-2", Position: 2, Status: model.WaitlistStatusWaiting},
		}, nil
	}}
	h := newWaitlistHandler(wl, activeRoomLookup(), wlBookingRepoStub{})

	rec := httptest.NewRecorder()
	h.listWaitlist(rec, wlRequest(http.MethodGet, "/rooms/room-1/waitlist", "room-1", "", memberActor))

	require.Equal(t, http.StatusOK, rec.Code)
	var got []model.WaitlistEntry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
}

// --- confirm --------------------------------------------------------------

func TestConfirmWaitlist_Created(t *testing.T) {
	offeredAt := time.Now().UTC()
	wl := wlRepoStub{
		getFn: func(context.Context, string) (model.WaitlistEntry, error) {
			return model.WaitlistEntry{
				ID: "wl-1", RoomID: "room-1", UserID: "user-1",
				StartTime: time.Now().Add(time.Hour), EndTime: time.Now().Add(2 * time.Hour),
				Status: model.WaitlistStatusOffered, OfferedAt: &offeredAt,
			}, nil
		},
		confirmAndBookFn: func(context.Context, string, model.Booking) (*model.Booking, error) {
			return nil, nil
		},
	}
	h := newWaitlistHandler(wl, activeRoomLookup(), wlBookingRepoStub{})

	rec := httptest.NewRecorder()
	h.confirmWaitlist(rec, wlRequest(http.MethodPost, "/waitlist/wl-1/confirm", "wl-1", "", memberActor))

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var got model.Booking
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, model.StatusConfirmed, got.Status)
}

func TestConfirmWaitlist_NotFound(t *testing.T) {
	wl := wlRepoStub{getFn: func(context.Context, string) (model.WaitlistEntry, error) {
		return model.WaitlistEntry{}, repository.ErrNotFound
	}}
	h := newWaitlistHandler(wl, activeRoomLookup(), wlBookingRepoStub{})

	rec := httptest.NewRecorder()
	h.confirmWaitlist(rec, wlRequest(http.MethodPost, "/waitlist/wl-x/confirm", "wl-x", "", memberActor))

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "WAITLIST_NOT_FOUND", errorCode(t, rec))
}

// --- leave ----------------------------------------------------------------

func TestLeaveWaitlist_NoContent(t *testing.T) {
	wl := wlRepoStub{
		getFn: func(context.Context, string) (model.WaitlistEntry, error) {
			return model.WaitlistEntry{ID: "wl-1", UserID: "user-1", Status: model.WaitlistStatusWaiting}, nil
		},
		deleteAndOfferFn: func(context.Context, string, time.Time) (*model.WaitlistEntry, error) { return nil, nil },
	}
	h := newWaitlistHandler(wl, activeRoomLookup(), wlBookingRepoStub{})

	rec := httptest.NewRecorder()
	h.leaveWaitlist(rec, wlRequest(http.MethodDelete, "/waitlist/wl-1", "wl-1", "", memberActor))

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestLeaveWaitlist_Forbidden(t *testing.T) {
	wl := wlRepoStub{getFn: func(context.Context, string) (model.WaitlistEntry, error) {
		return model.WaitlistEntry{ID: "wl-1", UserID: "user-other", Status: model.WaitlistStatusWaiting}, nil
	}}
	h := newWaitlistHandler(wl, activeRoomLookup(), wlBookingRepoStub{})

	rec := httptest.NewRecorder()
	h.leaveWaitlist(rec, wlRequest(http.MethodDelete, "/waitlist/wl-1", "wl-1", "", memberActor))

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "WAITLIST_FORBIDDEN", errorCode(t, rec))
}

// errorCode извлекает поле code из тела ошибки.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Code
}
