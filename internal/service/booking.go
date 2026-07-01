package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/example/booking-engine/internal/cache"
	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/tracing"
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
	GetBookingsByDateRange(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error)
}

// RoomLookup — доступ к комнатам, нужный сервису бронирования: точечный Get
// (Create/Cancel) и поиск свободных комнат Available (GetAvailableRooms).
type RoomLookup interface {
	Get(ctx context.Context, id string) (model.Room, error)
	Available(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error)
}

type Booking struct {
	rooms     RoomLookup
	bookings  BookingRepo
	cache     cache.RoomCacheInterface
	publisher events.EventPublisher
	topic     string
	log       *slog.Logger
	now       func() time.Time
}

// NewBooking собирает сервис бронирования. roomCache и publisher могут быть nil
// (тогда, соответственно, кэш доступности отключён — всегда идём в БД — и события
// не публикуются, например в окружении без Redis/Kafka); log == nil заменяется на
// slog.Default().
func NewBooking(rooms RoomLookup, bookings BookingRepo, roomCache cache.RoomCacheInterface, publisher events.EventPublisher, topic string, log *slog.Logger) *Booking {
	if log == nil {
		log = slog.Default()
	}
	return &Booking{
		rooms:     rooms,
		bookings:  bookings,
		cache:     roomCache,
		publisher: publisher,
		topic:     topic,
		log:       log,
		now:       time.Now,
	}
}

type BookingCreateInput struct {
	RoomID    string
	Title     string
	StartTime time.Time
	EndTime   time.Time
}

func (s *Booking) Create(ctx context.Context, a Actor, in BookingCreateInput) (model.Booking, error) {
	// Контекстные поля операции навешиваются один раз на входе и тянутся во все
	// её логи (booking_id добавляется ниже, когда бронь получает id).
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "room_id", in.RoomID)

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
		log.Error("create booking: room lookup", slog.Any("error", err))
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
	log = log.With("booking_id", b.ID)

	conflict, err := s.bookings.CreateChecked(ctx, b)
	if err != nil {
		log.Error("create booking: persist", slog.Any("error", err))
		return model.Booking{}, err
	}
	if conflict != nil {
		return model.Booking{}, &BookingConflictError{
			ConflictingID:    conflict.ID,
			ConflictingStart: conflict.StartTime,
			ConflictingEnd:   conflict.EndTime,
		}
	}

	log.Info("booking created")

	// Комната занята на новом интервале — список свободных комнат в затронутых
	// окнах устарел, сбрасываем кэш доступности.
	s.invalidateRoomCache(ctx, log, b.RoomID)

	// Бронь зафиксирована — публикуем событие. Сбой публикации не откатывает
	// операцию (eventual consistency), только логируется.
	s.publishEvent(ctx, log, events.TypeBookingCreated, b)
	return b, nil
}

func (s *Booking) Cancel(ctx context.Context, a Actor, id string) (model.Booking, error) {
	// room_id добавляется ниже, когда бронь прочитана из БД.
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "booking_id", id)

	b, err := s.bookings.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrBookingNotFound
		}
		log.Error("cancel booking: lookup", slog.Any("error", err))
		return model.Booking{}, err
	}
	log = log.With("room_id", b.RoomID)

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
		log.Error("cancel booking: persist", slog.Any("error", err))
		return model.Booking{}, err
	}
	b.Status = model.StatusCancelled

	log.Info("booking cancelled")

	// Комната освободилась на интервале брони — сбрасываем кэш доступности.
	s.invalidateRoomCache(ctx, log, b.RoomID)

	// Бронь отменена в БД — публикуем событие (см. Create про eventual consistency).
	s.publishEvent(ctx, log, events.TypeBookingCancelled, b)
	return b, nil
}

// GetAvailableRooms возвращает комнаты без подтверждённых броней в окне
// [start, end). Сначала пробуем кэш; при промахе идём в БД и кладём результат в
// кэш. Кэш — оптимизация: любые его ошибки логируются, но не роняют запрос
// (деградация к БД). При nil-кэше метод всегда обращается к БД.
func (s *Booking) GetAvailableRooms(ctx context.Context, _ Actor, start, end time.Time) ([]model.Room, error) {
	if !end.After(start) {
		return nil, ErrInvalidTimeRange
	}

	log := s.log.With("trace_id", tracing.TraceID(ctx))

	if s.cache != nil {
		switch rooms, err := s.cache.GetAvailableRooms(ctx, start, end); {
		case err == nil:
			return rooms, nil
		case errors.Is(err, cache.ErrCacheMiss):
			// промах — идём в БД
		default:
			log.Warn("room cache get, falling back to db", slog.Any("error", err)) // деградация, запрос не роняем
		}
	}

	rooms, err := s.rooms.Available(ctx, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), nil, nil, nil)
	if err != nil {
		log.Error("list available rooms", slog.Any("error", err))
		return nil, err
	}

	if s.cache != nil {
		if err := s.cache.SetAvailableRooms(ctx, start, end, rooms); err != nil {
			log.Warn("room cache set", slog.Any("error", err))
		}
	}
	return rooms, nil
}

// invalidateRoomCache сбрасывает кэш доступности после изменения броней комнаты.
// Кэш опционален и не критичен для корректности (TTL всё равно ограничивает
// рассогласование): ошибка — это деградация, на которой сервис выжил, поэтому
// уровень Warn, а не Error. No-op при nil-кэше. log уже несёт контекст операции.
func (s *Booking) invalidateRoomCache(ctx context.Context, log *slog.Logger, roomID string) {
	if s.cache == nil {
		return
	}
	if err := s.cache.InvalidateRoomCache(ctx, roomID); err != nil {
		log.Warn("invalidate room cache", slog.Any("error", err))
	}
}

// publishEvent публикует событие о брони после успешной записи в БД. Сбой
// публикации — деградация, а не провал операции: бронь уже зафиксирована,
// откатывать её из-за недоступной шины нельзя, поэтому уровень Warn. При
// nil-паблишере метод — no-op. log уже несёт контекст операции.
func (s *Booking) publishEvent(ctx context.Context, log *slog.Logger, eventType string, b model.Booking) {
	if s.publisher == nil {
		return
	}
	ev := events.Event{
		Type:      eventType,
		BookingID: b.ID,
		UserID:    b.UserID,
		RoomID:    b.RoomID,
		Timestamp: s.now(),
	}
	if err := s.publisher.Publish(ctx, s.topic, ev); err != nil {
		log.Warn("publish booking event, kafka unavailable",
			slog.Any("error", err),
			"type", eventType,
			"topic", s.topic,
		)
	}
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

// weekBounds нормализует weekStart к началу суток в UTC и возвращает полуоткрытый
// интервал недели [dayStart, weekEnd).
func weekBounds(weekStart time.Time) (dayStart, weekEnd time.Time) {
	w := weekStart.UTC()
	dayStart = time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart, dayStart.AddDate(0, 0, DaysPerWeek)
}

// dayIndex возвращает порядковый номер дня start_time относительно начала недели
// dayStart (0 = первый день). В UTC сутки ровно 24 часа, поэтому индекс однозначен.
func dayIndex(start, dayStart time.Time) int {
	return int(start.UTC().Sub(dayStart) / (24 * time.Hour))
}

// matchesBookingFilters сообщает, проходит ли бронь необязательные фильтры:
// nil-поле фильтра пропускает бронь с любым значением этого измерения.
func matchesBookingFilters(b model.Booking, f model.BookingFilters) bool {
	if f.Status != nil && string(b.Status) != *f.Status {
		return false
	}
	if f.UserID != nil && b.UserID != *f.UserID {
		return false
	}
	return true
}

// GetBookingsByWeek возвращает брони комнаты за неделю [weekStart, weekStart+7д),
// сгруппированные по дням start_time и отфильтрованные по filters. weekStart обязан
// быть понедельником — иначе ValidationError. Результат всегда содержит ровно
// DaysPerWeek элементов, по одному на день начиная с weekStart; несуществующая
// комната (нет броней в периоде) даёт неделю пустых дней без ошибки.
func (s *Booking) GetBookingsByWeek(ctx context.Context, roomID string, weekStart time.Time, filters model.BookingFilters) ([]model.DailyBookings, error) {
	if weekStart.UTC().Weekday() != time.Monday {
		return nil, &ValidationError{Field: "week_start", Message: "week_start must be a Monday"}
	}
	dayStart, weekEnd := weekBounds(weekStart)

	bookings, err := s.bookings.GetBookingsByDateRange(ctx, roomID, dayStart, weekEnd)
	if err != nil {
		return nil, err
	}

	days := make([]model.DailyBookings, DaysPerWeek)
	for i := range days {
		days[i] = model.DailyBookings{
			Date:     dayStart.AddDate(0, 0, i),
			Bookings: []model.Booking{},
		}
	}
	for _, b := range bookings {
		if !matchesBookingFilters(b, filters) {
			continue
		}
		idx := dayIndex(b.StartTime, dayStart)
		if idx < 0 || idx >= DaysPerWeek {
			continue // страховка: репозиторий такие записи не возвращает
		}
		days[idx].Bookings = append(days[idx].Bookings, b)
	}
	return days, nil
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

	dayStart, weekEnd := weekBounds(weekStart)

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
	for _, b := range bookings {
		idx := dayIndex(b.StartTime, dayStart)
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
