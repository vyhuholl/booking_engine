package service

import (
	"errors"
	"time"
)

var (
	ErrRoomNotFound          = errors.New("room_not_found")
	ErrBookingNotFound       = errors.New("booking_not_found")
	ErrUserNotFound          = errors.New("user_not_found")
	ErrValidation            = errors.New("validation_error")
	ErrInvalidTimeRange      = errors.New("invalid_time_range")
	ErrStartInPast           = errors.New("start_in_past")
	ErrDurationTooShort      = errors.New("duration_too_short")
	ErrDurationTooLong       = errors.New("duration_too_long")
	ErrRoomOutOfService      = errors.New("room_out_of_service")
	ErrCancelTooLate         = errors.New("cancel_too_late")
	ErrAlreadyCancelled      = errors.New("already_cancelled")
	ErrCancelForbidden       = errors.New("cancel_forbidden")
	ErrForbidden             = errors.New("forbidden")
	ErrRoomHasActiveBookings = errors.New("room_has_active_bookings")

	// Waitlist (лист ожидания).
	ErrRoomAvailable     = errors.New("room_available")      // комната свободна — нужна обычная бронь, не очередь
	ErrAlreadyInWaitlist = errors.New("already_in_waitlist") // уже есть активная запись на этот интервал
	ErrWaitlistNotFound  = errors.New("waitlist_not_found")
	ErrOfferNotPending   = errors.New("offer_not_pending") // запись не в статусе offered
	ErrOfferExpired      = errors.New("offer_expired")     // прошло больше OfferTTL с момента предложения
	ErrWaitlistForbidden = errors.New("waitlist_forbidden")

	// Approval workflow (одобрение больших переговорок, change add-large-room-approval).
	ErrApprovalNotFound   = errors.New("approval_not_found")   // брони на согласовании с таким id нет
	ErrNotPendingApproval = errors.New("not_pending_approval") // бронь не в статусе pending_approval (уже approved/rejected/cancelled/просрочена)
)

type BookingConflictError struct {
	ConflictingID    string
	ConflictingStart time.Time
	ConflictingEnd   time.Time
}

func (e *BookingConflictError) Error() string { return "booking_conflict" }

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }
