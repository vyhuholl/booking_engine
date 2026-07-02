package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/example/booking-engine/internal/service"
)

type waitlistJoinBody struct {
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
}

// joinWaitlist — POST /rooms/{id}/waitlist. Встать в очередь на занятый интервал.
func (h *Handler) joinWaitlist(w http.ResponseWriter, r *http.Request) {
	var body waitlistJoinBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body", nil)
		return
	}
	entry, err := h.waitlist.Join(r.Context(), actorFromCtx(r.Context()), service.WaitlistJoinInput{
		RoomID:    chi.URLParam(r, "id"),
		StartTime: body.StartTime,
		EndTime:   body.EndTime,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// listWaitlist — GET /rooms/{id}/waitlist. Посмотреть очередь комнаты.
func (h *Handler) listWaitlist(w http.ResponseWriter, r *http.Request) {
	entries, err := h.waitlist.ListByRoom(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// confirmWaitlist — POST /waitlist/{id}/confirm. Подтвердить предложенный слот.
func (h *Handler) confirmWaitlist(w http.ResponseWriter, r *http.Request) {
	booking, err := h.waitlist.Confirm(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, booking)
}

// leaveWaitlist — DELETE /waitlist/{id}. Выйти из очереди.
func (h *Handler) leaveWaitlist(w http.ResponseWriter, r *http.Request) {
	if err := h.waitlist.Leave(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
