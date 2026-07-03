package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// approvalRejectBody — тело POST /admin/approvals/{id}/reject.
type approvalRejectBody struct {
	Reason string `json:"reason"`
}

// listApprovals — GET /admin/approvals. Список броней, ожидающих одобрения.
// Авторизация (admin-only) — в сервисе.
func (h *Handler) listApprovals(w http.ResponseWriter, r *http.Request) {
	approvals, err := h.bookings.ListPendingApprovals(r.Context(), actorFromCtx(r.Context()))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approvals)
}

// approveBooking — POST /admin/approvals/{id}/approve. Одобрение брони.
// Авторизация — в сервисе.
func (h *Handler) approveBooking(w http.ResponseWriter, r *http.Request) {
	booking, err := h.bookings.Approve(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

// rejectBooking — POST /admin/approvals/{id}/reject с телом {"reason": "..."}.
// Отклонение брони с причиной. Авторизация и валидация reason — в сервисе.
func (h *Handler) rejectBooking(w http.ResponseWriter, r *http.Request) {
	var body approvalRejectBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body", nil)
		return
	}
	booking, err := h.bookings.Reject(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"), body.Reason)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}
