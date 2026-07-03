package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/example/booking-engine/internal/model"
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
	// Бронь большой переговорки создаётся на согласовании (pending_approval) —
	// отвечаем 202 Accepted вместо 201 Created (change add-large-room-approval).
	status := http.StatusCreated
	if booking.Status == model.StatusPendingApproval {
		status = http.StatusAccepted
	}
	writeJSON(w, status, booking)
}

func (h *Handler) cancelBooking(w http.ResponseWriter, r *http.Request) {
	booking, err := h.bookings.Cancel(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

// forceCancelBooking — DELETE /bookings/{id}/force. Принудительная отмена брони
// администратором: в отличие от обычной отмены игнорирует 30-минутный дедлайн
// (можно отменить даже уже начавшуюся бронь). Авторизация — в сервисе.
func (h *Handler) forceCancelBooking(w http.ResponseWriter, r *http.Request) {
	if _, err := h.bookings.ForceCancel(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
