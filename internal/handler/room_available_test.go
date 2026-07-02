package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/cache"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/service"
)

// --- Стабы зависимостей сервисов -----------------------------------------
// Встраиваем интерфейс сервисной зависимости и переопределяем только нужный
// метод; вызов любого другого (не ожидаемого в тесте) паникует разыменованием
// nil — это и ловит поход не по тому пути.

type roomRepoStub struct {
	service.RoomRepo
	availableFn func(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error)
}

func (s roomRepoStub) Available(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error) {
	return s.availableFn(ctx, start, end, capacityMin, floor, equipment)
}

type roomCacheStub struct {
	cache.RoomCacheInterface
	getFn func(ctx context.Context, start, end time.Time) ([]model.Room, error)
}

func (s roomCacheStub) GetAvailableRooms(ctx context.Context, start, end time.Time) ([]model.Room, error) {
	return s.getFn(ctx, start, end)
}

type (
	bookingsForRoomStub struct{ service.BookingsForRoom }
	roomLookupStub      struct{ service.RoomLookup }
	bookingRepoStub     struct{ service.BookingRepo }
	userLookupStub      struct{ UserLookup }
)

// newAvailabilityHandler собирает Handler, где кэшированный путь (Booking) отдаёт
// cachedRooms, а фильтруемый (Room.Available) — filteredRooms. По id в ответе
// видно, какой путь выбрал хендлер.
func newAvailabilityHandler(cachedRooms, filteredRooms []model.Room) *Handler {
	roomSvc := service.NewRoom(
		roomRepoStub{availableFn: func(context.Context, string, string, *int, *int, []string) ([]model.Room, error) {
			return filteredRooms, nil
		}},
		bookingsForRoomStub{},
	)
	bookingSvc := service.NewBooking(
		roomLookupStub{},
		bookingRepoStub{},
		roomCacheStub{getFn: func(context.Context, time.Time, time.Time) ([]model.Room, error) {
			return cachedRooms, nil
		}},
		nil, "", nil,
	)
	return New(slog.Default(), "secret", userLookupStub{}, roomSvc, bookingSvc, nil, nil)
}

func windowQuery() url.Values {
	v := url.Values{}
	v.Set("start", "2026-05-19T10:00:00Z")
	v.Set("end", "2026-05-19T11:00:00Z")
	return v
}

// doAvailable вызывает хендлер напрямую с актором в контексте (в обход
// authMiddleware) и возвращает распарсенный список комнат.
func doAvailable(t *testing.T, h *Handler, q url.Values) []model.Room {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/rooms/available?"+q.Encode(), nil)
	ctx := context.WithValue(req.Context(), actorKey, service.Actor{ID: "user-1", Role: model.RoleMember})
	rec := httptest.NewRecorder()
	h.searchAvailableRooms(rec, req.WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out []model.Room
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// Запрос без фильтров обслуживается кэшированным Booking.GetAvailableRooms.
func TestSearchAvailableRooms_UsesCacheWhenNoFilters(t *testing.T) {
	h := newAvailabilityHandler([]model.Room{{ID: "cached-1"}}, []model.Room{{ID: "filtered-1"}})

	got := doAvailable(t, h, windowQuery())
	require.Len(t, got, 1)
	assert.Equal(t, "cached-1", got[0].ID, "голый запрос должен идти через кэш")
}

// Любой фильтр уводит запрос в некэшируемый Room.Available (ключ кэша фильтры
// не различает).
func TestSearchAvailableRooms_UsesFilteredPathWhenFilterPresent(t *testing.T) {
	h := newAvailabilityHandler([]model.Room{{ID: "cached-1"}}, []model.Room{{ID: "filtered-1"}})

	for _, f := range []struct{ key, val string }{
		{"floor", "2"},
		{"capacity_min", "4"},
		{"equipment", "projector"},
	} {
		q := windowQuery()
		q.Set(f.key, f.val)
		got := doAvailable(t, h, q)
		require.Len(t, got, 1, "filter %s", f.key)
		assert.Equal(t, "filtered-1", got[0].ID, "фильтр %s должен идти мимо кэша", f.key)
	}
}
