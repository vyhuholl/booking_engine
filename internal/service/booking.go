package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
)

const (
	MinBookingDuration = 15 * time.Minute
	MaxBookingDuration = 8 * time.Hour
	CancelDeadline     = 30 * time.Minute
)

type BookingRepo interface {
	Get(ctx context.Context, id string) (model.Booking, error)
	CreateChecked(ctx context.Context, b model.Booking) (*model.Booking, error)
	Cancel(ctx context.Context, id string) error
	ListByUser(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error)
}

type RoomLookup interface {
	Get(ctx context.Context, id string) (model.Room, error)
}

type Booking struct {
	rooms    RoomLookup
	bookings BookingRepo
	now      func() time.Time
}

func NewBooking(rooms RoomLookup, bookings BookingRepo) *Booking {
	return &Booking{rooms: rooms, bookings: bookings, now: time.Now}
}

type BookingCreateInput struct {
	RoomID    string
	Title     string
	StartTime time.Time
	EndTime   time.Time
}

func (s *Booking) Create(ctx context.Context, a Actor, in BookingCreateInput) (model.Booking, error) {
	if strings.TrimSpace(in.Title) == "" {
		return model.Booking{}, &ValidationError{Field: "title", Message: "title is required"}
	}
	if strings.TrimSpace(in.RoomID) == "" {
		return model.Booking{}, &ValidationError{Field: "room_id", Message: "room_id is required"}
	}
	if in.StartTime.IsZero() || in.EndTime.IsZero() {
		return model.Booking{}, &ValidationError{Field: "start_time", Message: "start_time and end_time are required"}
	}
	if !in.EndTime.After(in.StartTime) {
		return model.Booking{}, ErrInvalidTimeRange
	}
	duration := in.EndTime.Sub(in.StartTime)
	if duration < MinBookingDuration {
		return model.Booking{}, ErrDurationTooShort
	}
	if duration > MaxBookingDuration {
		return model.Booking{}, ErrDurationTooLong
	}

	if _, err := s.rooms.Get(ctx, in.RoomID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrRoomNotFound
		}
		return model.Booking{}, err
	}

	b := model.Booking{
		ID:        "b-" + uuid.NewString(),
		RoomID:    in.RoomID,
		UserID:    a.ID,
		Title:     strings.TrimSpace(in.Title),
		StartTime: in.StartTime.UTC(),
		EndTime:   in.EndTime.UTC(),
		Status:    model.StatusConfirmed,
	}

	conflict, err := s.bookings.CreateChecked(ctx, b)
	if err != nil {
		return model.Booking{}, err
	}
	if conflict != nil {
		return model.Booking{}, &BookingConflictError{
			ConflictingID:    conflict.ID,
			ConflictingStart: conflict.StartTime,
			ConflictingEnd:   conflict.EndTime,
		}
	}
	return b, nil
}

func (s *Booking) Cancel(ctx context.Context, a Actor, id string) (model.Booking, error) {
	b, err := s.bookings.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrBookingNotFound
		}
		return model.Booking{}, err
	}

	if b.Status == model.StatusCancelled {
		return model.Booking{}, ErrAlreadyCancelled
	}

	if err := s.checkCancelPermission(ctx, a, b); err != nil {
		return model.Booking{}, err
	}

	if b.StartTime.Sub(s.now()) < CancelDeadline {
		return model.Booking{}, ErrCancelTooLate
	}

	if err := s.bookings.Cancel(ctx, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// гонка: уже отменили между Get и Cancel
			return model.Booking{}, ErrAlreadyCancelled
		}
		return model.Booking{}, err
	}
	b.Status = model.StatusCancelled
	return b, nil
}

func (s *Booking) checkCancelPermission(ctx context.Context, a Actor, b model.Booking) error {
	if a.IsAdmin() || b.UserID == a.ID {
		return nil
	}
	if a.IsManager() {
		if a.ManagesFloor == nil {
			return ErrCancelForbidden
		}
		room, err := s.rooms.Get(ctx, b.RoomID)
		if err != nil {
			// Если комната пропала — отменять чужое не даём.
			return ErrCancelForbidden
		}
		if room.Floor == *a.ManagesFloor {
			return nil
		}
	}
	return ErrCancelForbidden
}

type UserBookingsQuery struct {
	UserID string
	Status *model.BookingStatus
	From   *time.Time
	To     *time.Time
}

func (s *Booking) ListByUser(ctx context.Context, a Actor, q UserBookingsQuery) ([]model.Booking, error) {
	if q.UserID != a.ID && !a.IsAdmin() && !a.IsManager() {
		return nil, ErrForbidden
	}
	return s.bookings.ListByUser(ctx, repository.UserBookingFilter{
		UserID: q.UserID,
		Status: q.Status,
		From:   q.From,
		To:     q.To,
	})
}
