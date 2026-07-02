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

// OfferTTL — окно, в течение которого пользователь обязан подтвердить предложенный
// (offered) слот. По истечении запись протухает (expired), слот предлагается следующему.
const OfferTTL = 15 * time.Minute

// WaitlistRepo — доступ к листу ожидания, нужный сервису (объявлен в потребителе).
type WaitlistRepo interface {
	Create(ctx context.Context, e model.WaitlistEntry) (model.WaitlistEntry, error)
	Get(ctx context.Context, id string) (model.WaitlistEntry, error)
	ListByRoom(ctx context.Context, roomID string) ([]model.WaitlistEntry, error)
	DeleteAndOfferNext(ctx context.Context, id string, now time.Time) (*model.WaitlistEntry, error)
	ConfirmAndBook(ctx context.Context, entryID string, b model.Booking) (*model.Booking, error)
	ExpireAndOfferNext(ctx context.Context, entryID string, now time.Time) (*model.WaitlistEntry, error)
}

type Waitlist struct {
	rooms     RoomLookup
	waitlist  WaitlistRepo
	bookings  BookingRepo
	cache     cache.RoomCacheInterface
	publisher events.EventPublisher
	topic     string
	log       *slog.Logger
	now       func() time.Time
}

// NewWaitlist собирает сервис листа ожидания. roomCache и publisher могут быть nil
// (кэш/события отключены, как в NewBooking); log == nil заменяется на slog.Default().
func NewWaitlist(rooms RoomLookup, waitlist WaitlistRepo, bookings BookingRepo, roomCache cache.RoomCacheInterface, publisher events.EventPublisher, topic string, log *slog.Logger) *Waitlist {
	if log == nil {
		log = slog.Default()
	}
	return &Waitlist{
		rooms:     rooms,
		waitlist:  waitlist,
		bookings:  bookings,
		cache:     roomCache,
		publisher: publisher,
		topic:     topic,
		log:       log,
		now:       time.Now,
	}
}

type WaitlistJoinInput struct {
	RoomID    string
	StartTime time.Time
	EndTime   time.Time
}

// Join ставит пользователя в очередь на занятый интервал комнаты. Запись создаётся
// только если комната занята подтверждённой бронью на этот интервал; свободная
// комната → ErrRoomAvailable (нужна обычная бронь). Интервал проходит те же проверки,
// что и обычная бронь. Дубликат активной записи пользователя → ErrAlreadyInWaitlist.
func (s *Waitlist) Join(ctx context.Context, a Actor, in WaitlistJoinInput) (model.WaitlistEntry, error) {
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "room_id", in.RoomID)

	if strings.TrimSpace(in.RoomID) == "" {
		return model.WaitlistEntry{}, &ValidationError{Field: "room_id", Message: "room_id is required"}
	}
	if err := validateInterval(s.now(), in.StartTime, in.EndTime); err != nil {
		return model.WaitlistEntry{}, err
	}

	room, err := s.rooms.Get(ctx, in.RoomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.WaitlistEntry{}, ErrRoomNotFound
		}
		log.Error("join waitlist: room lookup", slog.Any("error", err))
		return model.WaitlistEntry{}, err
	}
	if room.Status == model.RoomStatusOutOfService {
		return model.WaitlistEntry{}, ErrRoomOutOfService
	}

	// Быстрый путь с понятной ошибкой: waitlist имеет смысл только на занятый
	// интервал. Это лишь предпроверка — окончательное решение принимается атомарно
	// внутри Create (см. ErrNoOverlap ниже), чтобы отмена брони в момент между этой
	// проверкой и вставкой не создала запись на уже свободный слот.
	busy, err := s.bookings.IsRoomBusy(ctx, in.RoomID, in.StartTime.UTC(), in.EndTime.UTC())
	if err != nil {
		log.Error("join waitlist: busy check", slog.Any("error", err))
		return model.WaitlistEntry{}, err
	}
	if !busy {
		return model.WaitlistEntry{}, ErrRoomAvailable
	}

	entry := model.WaitlistEntry{
		ID:        "wl-" + uuid.NewString(),
		RoomID:    in.RoomID,
		UserID:    a.ID,
		StartTime: in.StartTime.UTC(),
		EndTime:   in.EndTime.UTC(),
		Status:    model.WaitlistStatusWaiting,
		CreatedAt: s.now().UTC(),
	}

	created, err := s.waitlist.Create(ctx, entry)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrConflict):
			return model.WaitlistEntry{}, ErrAlreadyInWaitlist
		case errors.Is(err, repository.ErrNoOverlap):
			// Гонка: комната освободилась между предпроверкой и вставкой.
			return model.WaitlistEntry{}, ErrRoomAvailable
		}
		log.Error("join waitlist: persist", slog.Any("error", err))
		return model.WaitlistEntry{}, err
	}

	log.Info("waitlist joined", "waitlist_id", created.ID, "position", created.Position)
	return created, nil
}

// ListByRoom возвращает очередь комнаты (записи, отсортированные по position).
func (s *Waitlist) ListByRoom(ctx context.Context, _ Actor, roomID string) ([]model.WaitlistEntry, error) {
	if strings.TrimSpace(roomID) == "" {
		return nil, &ValidationError{Field: "room_id", Message: "room_id is required"}
	}
	return s.waitlist.ListByRoom(ctx, roomID)
}

// Confirm подтверждает предложенный (offered) слот: атомарно создаёт обычную бронь и
// переводит запись в converted. Подтверждать может только владелец. Если с момента
// предложения прошло больше OfferTTL — запись протухает (expired), слот предлагается
// следующему, возвращается ErrOfferExpired. Гонка двух confirm: один успех, второй —
// ErrOfferNotPending.
func (s *Waitlist) Confirm(ctx context.Context, a Actor, id string) (model.Booking, error) {
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "waitlist_id", id)

	entry, err := s.waitlist.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrWaitlistNotFound
		}
		log.Error("confirm waitlist: lookup", slog.Any("error", err))
		return model.Booking{}, err
	}
	log = log.With("room_id", entry.RoomID)

	if entry.UserID != a.ID {
		return model.Booking{}, ErrWaitlistForbidden
	}
	if entry.Status != model.WaitlistStatusOffered {
		return model.Booking{}, ErrOfferNotPending
	}

	// Ленивая проверка таймаута: воркера нет, протухание фиксируется здесь.
	if entry.OfferedAt == nil || s.now().Sub(*entry.OfferedAt) > OfferTTL {
		offered, err := s.waitlist.ExpireAndOfferNext(ctx, id, s.now().UTC())
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				// гонка: запись уже не offered
				return model.Booking{}, ErrOfferNotPending
			}
			log.Error("confirm waitlist: expire", slog.Any("error", err))
			return model.Booking{}, err
		}
		log.Info("waitlist offer expired")
		if offered != nil {
			log.Info("waitlist slot offered",
				"offered_waitlist_id", offered.ID,
				"offered_user_id", offered.UserID,
				"position", offered.Position,
			)
		}
		return model.Booking{}, ErrOfferExpired
	}

	// Те же правила, что при обычном создании брони, применяются и к конвертации:
	// нельзя подтвердить слот, чьё начало уже наступило, или для комнаты, выведенной
	// из обслуживания после постановки в очередь.
	if !entry.StartTime.After(s.now()) {
		return model.Booking{}, ErrStartInPast
	}
	room, err := s.rooms.Get(ctx, entry.RoomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrRoomNotFound
		}
		log.Error("confirm waitlist: room lookup", slog.Any("error", err))
		return model.Booking{}, err
	}
	if room.Status == model.RoomStatusOutOfService {
		return model.Booking{}, ErrRoomOutOfService
	}

	b := model.Booking{
		ID:        "b-" + uuid.NewString(),
		RoomID:    entry.RoomID,
		UserID:    entry.UserID,
		Title:     "Waitlist booking",
		StartTime: entry.StartTime.UTC(),
		EndTime:   entry.EndTime.UTC(),
		Status:    model.StatusConfirmed,
	}
	log = log.With("booking_id", b.ID)

	conflict, err := s.waitlist.ConfirmAndBook(ctx, id, b)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// гонка: другой confirm уже погасил запись
			return model.Booking{}, ErrOfferNotPending
		}
		log.Error("confirm waitlist: persist", slog.Any("error", err))
		return model.Booking{}, err
	}
	if conflict != nil {
		return model.Booking{}, &BookingConflictError{
			ConflictingID:    conflict.ID,
			ConflictingStart: conflict.StartTime,
			ConflictingEnd:   conflict.EndTime,
		}
	}

	log.Info("waitlist confirmed")

	// Комната занята новой бронью — сбрасываем кэш доступности и публикуем событие
	// (как в Booking.Create; отдельного waitlist-события не вводим).
	invalidateRoomCache(ctx, s.cache, log, b.RoomID)
	publishBookingEvent(ctx, s.publisher, s.topic, log, events.TypeBookingCreated, b, s.now())
	return b, nil
}

// Leave убирает запись пользователя из очереди. Удалять может владелец или admin.
// Если снималась offered-запись, освободившийся слот в той же транзакции предлагается
// следующему в очереди — иначе выход владельца «подвесил» бы очередь на этот слот.
func (s *Waitlist) Leave(ctx context.Context, a Actor, id string) error {
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "waitlist_id", id)

	entry, err := s.waitlist.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrWaitlistNotFound
		}
		log.Error("leave waitlist: lookup", slog.Any("error", err))
		return err
	}

	if entry.UserID != a.ID && !a.IsAdmin() {
		return ErrWaitlistForbidden
	}

	offered, err := s.waitlist.DeleteAndOfferNext(ctx, id, s.now().UTC())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrWaitlistNotFound
		}
		log.Error("leave waitlist: persist", slog.Any("error", err))
		return err
	}

	log.Info("waitlist left")
	if offered != nil {
		log.Info("waitlist slot offered",
			"offered_waitlist_id", offered.ID,
			"offered_user_id", offered.UserID,
			"position", offered.Position,
		)
	}
	return nil
}
