package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/example/booking-engine/internal/service"
)

type bookingCreateBody struct {
	RoomID    string    `json:"room_id"`
	Title     string    `json:"title"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
}

func (h *Handler) createBooking(w http.ResponseWriter, r *http.Request) {
	var body bookingCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body", nil)
		return
	}
	booking, err := h.bookings.Create(r.Context(), actorFromCtx(r.Context()), service.BookingCreateInput{
		RoomID:    body.RoomID,
		Title:     body.Title,
		StartTime: body.StartTime,
		EndTime:   body.EndTime,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, booking)
}

func (h *Handler) cancelBooking(w http.ResponseWriter, r *http.Request) {
	booking, err := h.bookings.Cancel(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}
