package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
)

type roomCreateBody struct {
	Name      string            `json:"name"`
	Capacity  int               `json:"capacity"`
	Floor     int               `json:"floor"`
	Equipment []model.Equipment `json:"equipment"`
}

type roomUpdateBody struct {
	Name      *string            `json:"name"`
	Capacity  *int               `json:"capacity"`
	Floor     *int               `json:"floor"`
	Equipment *[]model.Equipment `json:"equipment"`
}

type roomListResponse struct {
	Items []model.Room `json:"items"`
	Total int          `json:"total"`
}

type roomStatsResponse struct {
	RoomID       string    `json:"room_id"`
	PeriodStart  time.Time `json:"period_start"`
	PeriodEnd    time.Time `json:"period_end"`
	BookingCount int       `json:"booking_count"`
}

type roomAvailabilityBody struct {
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
}

type roomAvailabilityResponse struct {
	Available bool            `json:"available"`
	Conflicts []model.Booking `json:"conflicts"`
}

func (h *Handler) listRooms(w http.ResponseWriter, r *http.Request) {
	f := repository.RoomFilter{
		Limit:  parseInt(r.URL.Query().Get("limit"), 50),
		Offset: parseInt(r.URL.Query().Get("offset"), 0),
	}
	if v := r.URL.Query().Get("floor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "floor must be an integer", nil)
			return
		}
		f.Floor = &n
	}
	if v := r.URL.Query().Get("min_capacity"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "min_capacity must be an integer", nil)
			return
		}
		f.MinCapacity = &n
	}
	items, total, err := h.rooms.List(r.Context(), actorFromCtx(r.Context()), f)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roomListResponse{Items: items, Total: total})
}

func (h *Handler) getRoom(w http.ResponseWriter, r *http.Request) {
	room, err := h.rooms.Get(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (h *Handler) createRoom(w http.ResponseWriter, r *http.Request) {
	var body roomCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body", nil)
		return
	}
	room, err := h.rooms.Create(r.Context(), actorFromCtx(r.Context()), service.RoomCreateInput{
		Name:      body.Name,
		Capacity:  body.Capacity,
		Floor:     body.Floor,
		Equipment: body.Equipment,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, room)
}

func (h *Handler) updateRoom(w http.ResponseWriter, r *http.Request) {
	var body roomUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body", nil)
		return
	}
	room, err := h.rooms.Update(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"),
		service.RoomUpdateInput{
			Name:      body.Name,
			Capacity:  body.Capacity,
			Floor:     body.Floor,
			Equipment: body.Equipment,
		})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (h *Handler) deleteRoom(w http.ResponseWriter, r *http.Request) {
	if err := h.rooms.Delete(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) searchAvailableRooms(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, err := parseRFC3339(q.Get("start"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid 'start': "+err.Error(), nil)
		return
	}
	end, err := parseRFC3339(q.Get("end"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid 'end': "+err.Error(), nil)
		return
	}
	query := service.AvailableQuery{Start: start, End: end}
	if v := q.Get("capacity_min"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "capacity_min must be an integer", nil)
			return
		}
		query.CapacityMin = &n
	}
	if v := q.Get("floor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "floor must be an integer", nil)
			return
		}
		query.Floor = &n
	}
	for _, e := range q["equipment"] {
		query.Equipment = append(query.Equipment, model.Equipment(e))
	}

	ctx := r.Context()
	actor := actorFromCtx(ctx)

	// Кэш доступности ключуется только окном [start, end) и не учитывает фильтры,
	// поэтому кэшированный путь берём лишь для «голого» запроса без фильтров;
	// с любым фильтром идём в некэшируемый Room.Available.
	var rooms []model.Room
	if query.CapacityMin == nil && query.Floor == nil && len(query.Equipment) == 0 {
		rooms, err = h.bookings.GetAvailableRooms(ctx, actor, start, end)
	} else {
		rooms, err = h.rooms.Available(ctx, actor, query)
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

func (h *Handler) listRoomBookings(w http.ResponseWriter, r *http.Request) {
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "'date' is required (YYYY-MM-DD)", nil)
		return
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid 'date' format, expected YYYY-MM-DD", nil)
		return
	}
	bookings, err := h.rooms.BookingsOnDate(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"), date)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bookings)
}

// checkRoomAvailability — POST /rooms/{id}/availability. Проверяет, свободна ли
// комната на интервале [start_time, end_time), и возвращает пересекающиеся брони.
func (h *Handler) checkRoomAvailability(w http.ResponseWriter, r *http.Request) {
	var body roomAvailabilityBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body", nil)
		return
	}
	res, err := h.rooms.CheckAvailability(r.Context(), actorFromCtx(r.Context()),
		chi.URLParam(r, "id"), body.StartTime, body.EndTime)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roomAvailabilityResponse{
		Available: res.Available,
		Conflicts: res.Conflicts,
	})
}

func (h *Handler) getRoomStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.rooms.Stats(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roomStatsResponse{
		RoomID:       stats.RoomID,
		PeriodStart:  stats.PeriodStart,
		PeriodEnd:    stats.PeriodEnd,
		BookingCount: stats.BookingCount,
	})
}

func parseInt(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func parseRFC3339(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, errEmpty
	}
	return time.Parse(time.RFC3339, v)
}

var errEmpty = stringError("value is required")

type stringError string

func (e stringError) Error() string { return string(e) }
