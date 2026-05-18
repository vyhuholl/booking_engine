package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/service"
)

func (h *Handler) listUserBookings(w http.ResponseWriter, r *http.Request) {
	q := service.UserBookingsQuery{UserID: chi.URLParam(r, "id")}

	if v := r.URL.Query().Get("status"); v != "" {
		s := model.BookingStatus(v)
		if s != model.StatusConfirmed && s != model.StatusCancelled {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"status must be 'confirmed' or 'cancelled'", nil)
			return
		}
		q.Status = &s
	}
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid 'from': "+err.Error(), nil)
			return
		}
		q.From = &t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid 'to': "+err.Error(), nil)
			return
		}
		q.To = &t
	}

	// Проверяем существование пользователя, чтобы вернуть 404, а не пустой список.
	if _, err := h.usersvc.Get(r.Context(), q.UserID); err != nil {
		writeServiceError(w, err)
		return
	}

	bookings, err := h.bookings.ListByUser(r.Context(), actorFromCtx(r.Context()), q)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bookings)
}
