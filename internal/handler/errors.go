package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/example/booking-engine/internal/service"
)

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	writeJSON(w, status, errorBody{Code: code, Message: message, Details: details})
}

// writeServiceError мапит доменные ошибки сервисного слоя на HTTP-ответы.
func writeServiceError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}

	// 400: валидация
	var verr *service.ValidationError
	if errors.As(err, &verr) {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", verr.Message,
			map[string]any{"field": verr.Field})
		return
	}
	switch {
	case errors.Is(err, service.ErrValidation):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request", nil)
		return
	case errors.Is(err, service.ErrInvalidTimeRange):
		writeError(w, http.StatusBadRequest, "INVALID_TIME_RANGE",
			"end_time must be after start_time", nil)
		return
	case errors.Is(err, service.ErrStartInPast):
		writeError(w, http.StatusBadRequest, "START_IN_PAST",
			"start_time must be in the future", nil)
		return
	case errors.Is(err, service.ErrDurationTooShort):
		writeError(w, http.StatusBadRequest, "BOOKING_DURATION_TOO_SHORT",
			"minimum booking duration is 15 minutes", nil)
		return
	case errors.Is(err, service.ErrDurationTooLong):
		writeError(w, http.StatusBadRequest, "BOOKING_DURATION_TOO_LONG",
			"maximum booking duration is 8 hours", nil)
		return
	}

	// 403
	switch {
	case errors.Is(err, service.ErrForbidden):
		writeError(w, http.StatusForbidden, "FORBIDDEN", "operation not allowed for your role", nil)
		return
	case errors.Is(err, service.ErrCancelForbidden):
		writeError(w, http.StatusForbidden, "CANCEL_FORBIDDEN",
			"you are not allowed to cancel this booking", nil)
		return
	case errors.Is(err, service.ErrWaitlistForbidden):
		writeError(w, http.StatusForbidden, "WAITLIST_FORBIDDEN",
			"you are not allowed to modify this waitlist entry", nil)
		return
	}

	// 404
	switch {
	case errors.Is(err, service.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "ROOM_NOT_FOUND", "room not found", nil)
		return
	case errors.Is(err, service.ErrBookingNotFound):
		writeError(w, http.StatusNotFound, "BOOKING_NOT_FOUND", "booking not found", nil)
		return
	case errors.Is(err, service.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "user not found", nil)
		return
	case errors.Is(err, service.ErrWaitlistNotFound):
		writeError(w, http.StatusNotFound, "WAITLIST_NOT_FOUND", "waitlist entry not found", nil)
		return
	}

	// 409
	var conflict *service.BookingConflictError
	if errors.As(err, &conflict) {
		writeError(w, http.StatusConflict, "BOOKING_CONFLICT",
			"room is already booked in the requested interval",
			map[string]any{
				"conflicting_booking_id": conflict.ConflictingID,
				"conflicting_start":      conflict.ConflictingStart.UTC().Format(time.RFC3339),
				"conflicting_end":        conflict.ConflictingEnd.UTC().Format(time.RFC3339),
			})
		return
	}
	switch {
	case errors.Is(err, service.ErrCancelTooLate):
		writeError(w, http.StatusConflict, "CANCEL_TOO_LATE",
			"cancellation is not allowed within 30 minutes of start", nil)
		return
	case errors.Is(err, service.ErrAlreadyCancelled):
		writeError(w, http.StatusConflict, "BOOKING_ALREADY_CANCELLED",
			"booking is already cancelled", nil)
		return
	case errors.Is(err, service.ErrRoomHasActiveBookings):
		writeError(w, http.StatusConflict, "ROOM_HAS_ACTIVE_BOOKINGS",
			"cannot delete room with active future bookings", nil)
		return
	case errors.Is(err, service.ErrRoomOutOfService):
		writeError(w, http.StatusConflict, "ROOM_OUT_OF_SERVICE",
			"room is not available for booking", nil)
		return
	case errors.Is(err, service.ErrRoomAvailable):
		writeError(w, http.StatusConflict, "WAITLIST_ROOM_AVAILABLE",
			"room is available in the requested interval; create a booking instead", nil)
		return
	case errors.Is(err, service.ErrAlreadyInWaitlist):
		writeError(w, http.StatusConflict, "ALREADY_IN_WAITLIST",
			"you already have a waitlist entry for this interval", nil)
		return
	case errors.Is(err, service.ErrOfferNotPending):
		writeError(w, http.StatusConflict, "OFFER_NOT_PENDING",
			"waitlist entry is not in offered state", nil)
		return
	case errors.Is(err, service.ErrOfferExpired):
		writeError(w, http.StatusConflict, "OFFER_EXPIRED",
			"the offered slot has expired", nil)
		return
	}

	writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error", nil)
}
