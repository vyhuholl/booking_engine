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

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
)

// --- Стабы для listRoomBookings ------------------------------------------
// BookingsOnDate сначала проверяет существование комнаты (RoomRepo.Get), затем
// тянет брони за дату (BookingsForRoom.ListByRoomOnDate). Стабим ровно эти два
// метода; остальные, если позовутся, паникуют разыменованием nil-интерфейса.

type roomGetStub struct {
	service.RoomRepo
	getFn func(ctx context.Context, id string) (model.Room, error)
}

func (s roomGetStub) Get(ctx context.Context, id string) (model.Room, error) {
	return s.getFn(ctx, id)
}

type roomBookingsStub struct {
	service.BookingsForRoom
	listFn func(ctx context.Context, roomID string, date time.Time) ([]model.Booking, error)
}

func (s roomBookingsStub) ListByRoomOnDate(ctx context.Context, roomID string, date time.Time) ([]model.Booking, error) {
	return s.listFn(ctx, roomID, date)
}

// newRoomBookingsHandler собирает Handler, где комната room найдена, а брони за
// дату отдаёт listFn (получает распарсенные roomID и date, чтобы тест мог их
// проверить). bookings-сервис в этом хендлере не используется — listRoomBookings
// ходит только в h.rooms.
func newRoomBookingsHandler(room model.Room, listFn func(ctx context.Context, roomID string, date time.Time) ([]model.Booking, error)) *Handler {
	roomSvc := service.NewRoom(
		roomGetStub{getFn: func(_ context.Context, id string) (model.Room, error) {
			if id == room.ID {
				return room, nil
			}
			return model.Room{}, repository.ErrNotFound
		}},
		roomBookingsStub{listFn: listFn},
	)
	return New(slog.Default(), "secret", userLookupStub{}, roomSvc, nil, nil, nil)
}

// doRoomBookings вызывает listRoomBookings напрямую с актором в контексте (в обход
// authMiddleware) и chi-параметром {id} в route context, возвращая рекордер.
func doRoomBookings(t *testing.T, h *Handler, roomID string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+roomID+"/bookings?"+q.Encode(), nil)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", roomID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, actorKey, service.Actor{ID: "u-member1", Role: model.RoleMember})

	rec := httptest.NewRecorder()
	h.listRoomBookings(rec, req.WithContext(ctx))
	return rec
}

// Реальные пользователи из БД (SELECT id, role FROM users) — их id фигурируют как
// user_id в бронях. Комнату сажаем на этаж менеджера u-mgr-3.
const (
	testAdminID   = "u-admin"
	testManagerID = "u-mgr-3"
	testMemberID  = "u-member1"
)

// TestListRoomBookings_ResponseFormat — основной тест: формат ответа 200 должен
// совпадать со схемой Booking из api/openapi.yaml (array из объектов с полями
// id, room_id, user_id, title, start_time, end_time, status; время — RFC3339).
func TestListRoomBookings_ResponseFormat(t *testing.T) {
	room := model.Room{ID: "room-301", Name: "Neptune", Floor: 3, Capacity: 8, Status: model.RoomStatusActive}
	date := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)

	// Две брони разных пользователей за одну дату — покрываем оба статуса enum'а.
	seeded := []model.Booking{
		{
			ID:        "b-100",
			RoomID:    room.ID,
			UserID:    testMemberID,
			Title:     "Sprint Planning",
			StartTime: date.Add(10 * time.Hour),
			EndTime:   date.Add(11 * time.Hour),
			Status:    model.StatusConfirmed,
		},
		{
			ID:        "b-101",
			RoomID:    room.ID,
			UserID:    testManagerID,
			Title:     "1:1",
			StartTime: date.Add(14 * time.Hour),
			EndTime:   date.Add(14*time.Hour + 30*time.Minute),
			Status:    model.StatusCancelled,
		},
	}

	var gotRoomID string
	var gotDate time.Time
	h := newRoomBookingsHandler(room, func(_ context.Context, roomID string, d time.Time) ([]model.Booking, error) {
		gotRoomID, gotDate = roomID, d
		return seeded, nil
	})

	q := url.Values{}
	q.Set("date", "2026-05-19")
	rec := doRoomBookings(t, h, room.ID, q)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	// Хендлер прокинул в сервис id комнаты из пути и распарсенную дату.
	assert.Equal(t, room.ID, gotRoomID)
	assert.Equal(t, date, gotDate)

	// 1) Тело — JSON-массив. Каждый элемент содержит РОВНО набор полей из схемы
	//    Booking (required: id, room_id, user_id, title, start_time, end_time, status).
	var raw []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw, len(seeded))

	wantKeys := []string{"id", "room_id", "user_id", "title", "start_time", "end_time", "status"}
	for i, obj := range raw {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, wantKeys, keys, "элемент %d: набор JSON-полей должен совпадать со схемой Booking", i)
	}

	// 2) Время сериализовано как RFC3339 date-time в UTC (суффикс Z) — как в примерах спеки.
	var firstStart string
	require.NoError(t, json.Unmarshal(raw[0]["start_time"], &firstStart))
	assert.Equal(t, "2026-05-19T10:00:00Z", firstStart)
	_, err := time.Parse(time.RFC3339, firstStart)
	assert.NoError(t, err, "start_time должен парситься как RFC3339")

	// 3) Значения полей соответствуют отданным сервисом бронями (порядок сохранён).
	var got []model.Booking
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, seeded, got)

	// 4) status — только допустимые значения enum'а BookingStatus.
	for _, b := range got {
		assert.Contains(t, []model.BookingStatus{model.StatusConfirmed, model.StatusCancelled}, b.Status)
	}
}

// Пустой день — валидный ответ: JSON-массив, а не null/объект ошибки.
func TestListRoomBookings_EmptyIsJSONArray(t *testing.T) {
	room := model.Room{ID: "room-301", Status: model.RoomStatusActive}
	h := newRoomBookingsHandler(room, func(context.Context, string, time.Time) ([]model.Booking, error) {
		return []model.Booking{}, nil
	})

	q := url.Values{}
	q.Set("date", "2026-05-19")
	rec := doRoomBookings(t, h, room.ID, q)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, "[]", rec.Body.String())
}

// Параметр date обязателен и обязан иметь формат YYYY-MM-DD — иначе 400 VALIDATION_ERROR
// (проверка в хендлере, до похода в сервис).
func TestListRoomBookings_DateValidation(t *testing.T) {
	room := model.Room{ID: "room-301", Status: model.RoomStatusActive}
	h := newRoomBookingsHandler(room, func(context.Context, string, time.Time) ([]model.Booking, error) {
		t.Fatal("сервис не должен вызываться при невалидном date")
		return nil, nil
	})

	t.Run("date отсутствует", func(t *testing.T) {
		rec := doRoomBookings(t, h, room.ID, url.Values{})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
	})

	t.Run("date в неверном формате", func(t *testing.T) {
		q := url.Values{}
		q.Set("date", "19-05-2026")
		rec := doRoomBookings(t, h, room.ID, q)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
	})
}

// Несуществующая комната → 404 ROOM_NOT_FOUND (доменная ошибка сервиса, смапленная в HTTP).
func TestListRoomBookings_RoomNotFound(t *testing.T) {
	room := model.Room{ID: "room-301", Status: model.RoomStatusActive}
	h := newRoomBookingsHandler(room, func(context.Context, string, time.Time) ([]model.Booking, error) {
		t.Fatal("не должны читать брони несуществующей комнаты")
		return nil, nil
	})

	q := url.Values{}
	q.Set("date", "2026-05-19")
	rec := doRoomBookings(t, h, "room-does-not-exist", q)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "ROOM_NOT_FOUND")
}
