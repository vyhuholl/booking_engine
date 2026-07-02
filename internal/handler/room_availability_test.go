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

// Стабы зависимостей Room-сервиса для проверки availability: реализуем ровно те
// методы, которые дёргает CheckAvailability (Get у комнат, ListConflicting у броней);
// любой другой вызов паникует разыменованием nil-интерфейса.

type availRoomRepoStub struct {
	service.RoomRepo
	getFn func(ctx context.Context, id string) (model.Room, error)
}

func (s availRoomRepoStub) Get(ctx context.Context, id string) (model.Room, error) {
	return s.getFn(ctx, id)
}

type availBookingsStub struct {
	service.BookingsForRoom
	listConflictingFn func(ctx context.Context, roomID string, start, end time.Time) ([]model.Booking, error)
}

func (s availBookingsStub) ListConflicting(ctx context.Context, roomID string, start, end time.Time) ([]model.Booking, error) {
	return s.listConflictingFn(ctx, roomID, start, end)
}

// newCheckAvailabilityHandler собирает Handler, чей Room-сервис отдаёт заданную
// комнату (room/roomErr) и заданный список пересечений (conflicts).
func newCheckAvailabilityHandler(room model.Room, roomErr error, conflicts []model.Booking) *Handler {
	roomSvc := service.NewRoom(
		availRoomRepoStub{getFn: func(context.Context, string) (model.Room, error) {
			return room, roomErr
		}},
		availBookingsStub{listConflictingFn: func(context.Context, string, time.Time, time.Time) ([]model.Booking, error) {
			return conflicts, nil
		}},
	)
	return New(slog.Default(), "secret", userLookupStub{}, roomSvc, nil, nil, nil)
}

const availabilityBody = `{"start_time":"2026-05-19T10:00:00Z","end_time":"2026-05-19T11:00:00Z"}`

func decodeAvailability(t *testing.T, rec *httptest.ResponseRecorder) roomAvailabilityResponse {
	t.Helper()
	var out roomAvailabilityResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestCheckRoomAvailability_Available(t *testing.T) {
	h := newCheckAvailabilityHandler(
		model.Room{ID: "room-1", Status: model.RoomStatusActive}, nil, []model.Booking{},
	)

	rec := httptest.NewRecorder()
	h.checkRoomAvailability(rec, wlRequest(http.MethodPost, "/rooms/room-1/availability", "room-1", availabilityBody, memberActor))

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got := decodeAvailability(t, rec)
	assert.True(t, got.Available)
	assert.Empty(t, got.Conflicts)
	assert.NotNil(t, got.Conflicts, "conflicts должен рендериться как [], не null")
}

func TestCheckRoomAvailability_ConflictsMakeUnavailable(t *testing.T) {
	conflict := model.Booking{ID: "b-1", RoomID: "room-1", Status: model.StatusConfirmed}
	h := newCheckAvailabilityHandler(
		model.Room{ID: "room-1", Status: model.RoomStatusActive}, nil, []model.Booking{conflict},
	)

	rec := httptest.NewRecorder()
	h.checkRoomAvailability(rec, wlRequest(http.MethodPost, "/rooms/room-1/availability", "room-1", availabilityBody, memberActor))

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got := decodeAvailability(t, rec)
	assert.False(t, got.Available)
	require.Len(t, got.Conflicts, 1)
	assert.Equal(t, "b-1", got.Conflicts[0].ID)
}

func TestCheckRoomAvailability_InvalidJSON(t *testing.T) {
	h := newCheckAvailabilityHandler(model.Room{ID: "room-1", Status: model.RoomStatusActive}, nil, nil)

	rec := httptest.NewRecorder()
	h.checkRoomAvailability(rec, wlRequest(http.MethodPost, "/rooms/room-1/availability", "room-1", "{not json", memberActor))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "VALIDATION_ERROR", errorCode(t, rec))
}

func TestCheckRoomAvailability_RoomNotFound(t *testing.T) {
	h := newCheckAvailabilityHandler(model.Room{}, repository.ErrNotFound, nil)

	rec := httptest.NewRecorder()
	h.checkRoomAvailability(rec, wlRequest(http.MethodPost, "/rooms/ghost/availability", "ghost", availabilityBody, memberActor))

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "ROOM_NOT_FOUND", errorCode(t, rec))
}
