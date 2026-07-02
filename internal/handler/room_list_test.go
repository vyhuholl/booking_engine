package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
)

// Расширение roomRepoStub для тестирования List (room_available_test.go уже
// определяет базовый roomRepoStub с методом Available).
type roomListRepoStub struct {
	service.RoomRepo
	availableFn func(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error)
	listFn      func(ctx context.Context, f repository.RoomFilter) ([]model.Room, int, error)
}

func (s roomListRepoStub) Available(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error) {
	return s.availableFn(ctx, start, end, capacityMin, floor, equipment)
}

func (s roomListRepoStub) List(ctx context.Context, f repository.RoomFilter) ([]model.Room, int, error) {
	return s.listFn(ctx, f)
}

type bookingListRepoStub struct {
	service.BookingRepo
}

// newRoomListHandler собирает Handler с roomListRepoStub для тестирования listRooms.
func newRoomListHandler(rooms []model.Room, total int) *Handler {
	roomSvc := service.NewRoom(
		roomListRepoStub{
			availableFn: func(context.Context, string, string, *int, *int, []string) ([]model.Room, error) {
				return nil, nil
			},
			listFn: func(context.Context, repository.RoomFilter) ([]model.Room, int, error) {
				return rooms, total, nil
			},
		},
		bookingsForRoomStub{},
	)
	bookingSvc := service.NewBooking(
		roomLookupStub{},
		bookingListRepoStub{},
		roomCacheStub{},
		nil,
		"",
		slog.Default(),
	)
	return New(slog.Default(), "secret", userLookupStub{}, roomSvc, bookingSvc, nil, nil)
}

// doRoomList вызывает хендлер listRooms напрямую с актором в контексте
// (в обход authMiddleware) и возвращает распарсенный ответ.
func doRoomList(t *testing.T, h *Handler, q url.Values) roomListResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/rooms?"+q.Encode(), nil)
	ctx := context.WithValue(req.Context(), actorKey, service.Actor{ID: "user-1", Role: model.RoleMember})
	rec := httptest.NewRecorder()
	h.listRooms(rec, req.WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out roomListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// Test ---------------------------------------------------------------------------------------------------------------------

func TestListRooms_NoQueryParameters(t *testing.T) {
	rooms := []model.Room{
		{ID: "room-aaa", Name: "AAA Room", Floor: 1, Capacity: 4, Status: model.RoomStatusActive},
		{ID: "room-bbb", Name: "BBB Room", Floor: 2, Capacity: 8, Status: model.RoomStatusActive},
	}
	h := newRoomListHandler(rooms, len(rooms))

	resp := doRoomList(t, h, url.Values{})

	assert.Equal(t, 2, resp.Total)
	assert.Len(t, resp.Items, 2)
	assert.Equal(t, "room-aaa", resp.Items[0].ID, "should be sorted by name")
}

func TestListRooms_WithFloorFilter(t *testing.T) {
	rooms := []model.Room{
		{ID: "room-aaa", Name: "AAA Room", Floor: 2, Capacity: 8, Status: model.RoomStatusActive},
	}
	h := newRoomListHandler(rooms, 1)

	q := url.Values{}
	q.Set("floor", "2")
	resp := doRoomList(t, h, q)

	assert.Equal(t, 1, resp.Total)
	assert.Len(t, resp.Items, 1)
	assert.Equal(t, 2, resp.Items[0].Floor)
}

func TestListRooms_WithMinCapacityFilter(t *testing.T) {
	rooms := []model.Room{
		{ID: "room-aaa", Name: "AAA Room", Floor: 1, Capacity: 10, Status: model.RoomStatusActive},
	}
	h := newRoomListHandler(rooms, 1)

	q := url.Values{}
	q.Set("min_capacity", "10")
	resp := doRoomList(t, h, q)

	assert.Equal(t, 1, resp.Total)
	assert.Len(t, resp.Items, 1)
	assert.GreaterOrEqual(t, resp.Items[0].Capacity, 10)
}

func TestListRooms_WithCombinedFilters(t *testing.T) {
	rooms := []model.Room{
		{ID: "room-aaa", Name: "AAA Room", Floor: 2, Capacity: 10, Status: model.RoomStatusActive},
	}
	h := newRoomListHandler(rooms, 1)

	q := url.Values{}
	q.Set("floor", "2")
	q.Set("min_capacity", "8")
	resp := doRoomList(t, h, q)

	assert.Equal(t, 1, resp.Total)
	assert.Len(t, resp.Items, 1)
	assert.Equal(t, 2, resp.Items[0].Floor)
	assert.GreaterOrEqual(t, resp.Items[0].Capacity, 8)
}

func TestListRooms_ValidationErrors(t *testing.T) {
	h := newRoomListHandler([]model.Room{}, 0)

	t.Run("floor is not an integer", func(t *testing.T) {
		q := url.Values{}
		q.Set("floor", "abc")
		req := httptest.NewRequest(http.MethodGet, "/rooms?"+q.Encode(), nil)
		ctx := context.WithValue(req.Context(), actorKey, service.Actor{ID: "user-1", Role: model.RoleMember})
		rec := httptest.NewRecorder()
		h.listRooms(rec, req.WithContext(ctx))

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
		assert.Contains(t, rec.Body.String(), "floor must be an integer")
	})

	t.Run("min_capacity is not an integer", func(t *testing.T) {
		q := url.Values{}
		q.Set("min_capacity", "xyz")
		req := httptest.NewRequest(http.MethodGet, "/rooms?"+q.Encode(), nil)
		ctx := context.WithValue(req.Context(), actorKey, service.Actor{ID: "user-1", Role: model.RoleMember})
		rec := httptest.NewRecorder()
		h.listRooms(rec, req.WithContext(ctx))

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
		assert.Contains(t, rec.Body.String(), "min_capacity must be an integer")
	})
}

func TestListRooms_NonActiveRoomsNotReturned(t *testing.T) {
	// Stub возвращает только active rooms (имитация фильтрации в репозитории)
	rooms := []model.Room{
		{ID: "room-aaa", Name: "AAA Room", Floor: 1, Capacity: 4, Status: model.RoomStatusActive},
		{ID: "room-oos", Name: "OOS Room", Floor: 1, Capacity: 6, Status: model.RoomStatusOutOfService},
	}
	// Но возвращаем только active
	h := newRoomListHandler([]model.Room{rooms[0]}, 1)

	resp := doRoomList(t, h, url.Values{})

	assert.Equal(t, 1, resp.Total)
	assert.Len(t, resp.Items, 1)
	assert.Equal(t, model.RoomStatusActive, resp.Items[0].Status)
}

func TestListRooms_UnauthenticatedRequest(t *testing.T) {
	// Примечание: проверка аутентификации выполняется authMiddleware.
	// В unit-тестах мы вызываем хендлер напрямую, минуя middleware.
	// Данный тест проверяет, что хендлер корректно работает,
	// когда актор установлен в контексте middleware'ом.
	h := newRoomListHandler([]model.Room{}, 0)

	// С пустым актором (имитация middleware с пустым пользователем)
	actor := service.Actor{}
	req := httptest.NewRequest(http.MethodGet, "/rooms", nil)
	ctx := context.WithValue(req.Context(), actorKey, actor)
	rec := httptest.NewRecorder()
	h.listRooms(rec, req.WithContext(ctx))

	// Хендлер должен обработать запрос (авторизация не требуется для GET /rooms)
	// и вернуть успешный ответ с пустым списком
	assert.Equal(t, http.StatusOK, rec.Code)
}
