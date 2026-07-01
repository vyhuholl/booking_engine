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

	// DaysPerWeek — размер окна недельного отчёта (WeeklyReport.Days).
	DaysPerWeek = 7
)

type BookingRepo interface {
	Get(ctx context.Context, id string) (model.Booking, error)
	CreateChecked(ctx context.Context, b model.Booking) (*model.Booking, error)
	Cancel(ctx context.Context, id string) error
	ListByUser(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error)
	ListByRoomInPeriod(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error)
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
	if !in.StartTime.After(s.now()) {
		return model.Booking{}, ErrStartInPast
	}
	duration := in.EndTime.Sub(in.StartTime)
	if duration < MinBookingDuration {
		return model.Booking{}, ErrDurationTooShort
	}
	if duration > MaxBookingDuration {
		return model.Booking{}, ErrDurationTooLong
	}

	room, err := s.rooms.Get(ctx, in.RoomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrRoomNotFound
		}
		return model.Booking{}, err
	}
	if room.Status == model.RoomStatusOutOfService {
		return model.Booking{}, ErrRoomOutOfService
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

// DayBookings — бронирования одного дня недельного отчёта.
type DayBookings struct {
	Date     time.Time       // начало суток в UTC
	Bookings []model.Booking // брони, начинающиеся в этот день (может быть пустым, не nil)
}

// WeeklyReport — отчёт по бронированиям комнаты за неделю [WeekStart, WeekEnd),
// сгруппированный по дням. Days всегда содержит ровно DaysPerWeek элементов,
// по одному на каждый день, начиная с WeekStart.
type WeeklyReport struct {
	RoomID    string
	WeekStart time.Time
	WeekEnd   time.Time // эксклюзивная граница: WeekStart + 7 суток
	Days      []DayBookings
}

// GetWeeklyReport возвращает бронирования комнаты за неделю, начинающуюся с weekStart,
// сгруппированные по дням. weekStart нормализуется к началу суток в UTC. Как и остальные
// отчёты об использовании комнаты, учитываются брони в любом статусе; группировка идёт
// по дню start_time. Права не проверяются — отчёт доступен любому аутентифицированному
// пользователю (аналогично Room.Stats/BookingsOnDate).
func (s *Booking) GetWeeklyReport(ctx context.Context, _ Actor, roomID string, weekStart time.Time) (WeeklyReport, error) {
	if strings.TrimSpace(roomID) == "" {
		return WeeklyReport{}, &ValidationError{Field: "room_id", Message: "room_id is required"}
	}
	if weekStart.IsZero() {
		return WeeklyReport{}, &ValidationError{Field: "week_start", Message: "week_start is required"}
	}

	if _, err := s.rooms.Get(ctx, roomID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return WeeklyReport{}, ErrRoomNotFound
		}
		return WeeklyReport{}, err
	}

	weekStart = weekStart.UTC()
	dayStart := time.Date(weekStart.Year(), weekStart.Month(), weekStart.Day(), 0, 0, 0, 0, time.UTC)
	weekEnd := dayStart.AddDate(0, 0, DaysPerWeek)

	bookings, err := s.bookings.ListByRoomInPeriod(ctx, roomID, dayStart, weekEnd)
	if err != nil {
		return WeeklyReport{}, err
	}

	days := make([]DayBookings, DaysPerWeek)
	for i := range days {
		days[i] = DayBookings{
			Date:     dayStart.AddDate(0, 0, i),
			Bookings: []model.Booking{},
		}
	}
	// В UTC нет переходов на летнее время, поэтому сутки ровно 24 часа и индекс дня
	// однозначно определяется смещением start_time от начала недели.
	for _, b := range bookings {
		idx := int(b.StartTime.UTC().Sub(dayStart) / (24 * time.Hour))
		if idx < 0 || idx >= DaysPerWeek {
			continue // страховка: репозиторий такие записи не возвращает
		}
		days[idx].Bookings = append(days[idx].Bookings, b)
	}

	return WeeklyReport{
		RoomID:    roomID,
		WeekStart: dayStart,
		WeekEnd:   weekEnd,
		Days:      days,
	}, nil
}
