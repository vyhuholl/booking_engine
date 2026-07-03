package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

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

	// MaxTitleLength — предел длины заголовка брони в символах (рунах, не байтах),
	// синхронизирован с maxLength в схеме BookingCreate (api/openapi.yaml).
	MaxTitleLength = 200

	// DaysPerWeek — размер окна недельного отчёта (WeeklyReport.Days).
	DaysPerWeek = 7

	// LargeRoomCapacityThreshold — комнаты вместимостью СТРОГО больше требуют
	// одобрения администратора: бронь создаётся в статусе pending_approval
	// (change add-large-room-approval).
	LargeRoomCapacityThreshold = 12

	// ApprovalTimeout — окно, в течение которого admin должен рассмотреть бронь на
	// согласовании; по истечении она лениво авто-отклоняется (без cron, как OfferTTL).
	ApprovalTimeout = 24 * time.Hour
)

// ApprovalTimeoutReason — причина, сохраняемая при авто-отклонении брони по таймауту.
const ApprovalTimeoutReason = "approval timeout exceeded"

type BookingRepo interface {
	Get(ctx context.Context, id string) (model.Booking, error)
	CreateChecked(ctx context.Context, b model.Booking) (*model.Booking, error)
	Cancel(ctx context.Context, id string) error
	CancelAndOfferWaitlist(ctx context.Context, id string, now time.Time) (model.Booking, *model.WaitlistEntry, error)
	IsRoomBusy(ctx context.Context, roomID string, start, end time.Time) (bool, error)
	ListByUser(ctx context.Context, f repository.UserBookingFilter) ([]model.Booking, error)
	ListByRoomInPeriod(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error)
	GetBookingsByDateRange(ctx context.Context, roomID string, from, to time.Time) ([]model.Booking, error)

	// Approval workflow (change add-large-room-approval).
	Approve(ctx context.Context, id string, now time.Time) (model.Booking, error)
	RejectAndOfferWaitlist(ctx context.Context, id, reason string, now time.Time) (model.Booking, *model.WaitlistEntry, error)
	ListPendingApprovals(ctx context.Context, now time.Time, timeout time.Duration, reason string) ([]model.Booking, []model.Booking, error)
}

// validateInterval проверяет временной интервал брони или waitlist-записи по общим
// правилам: обе границы заданы, end > start, start в будущем относительно now,
// длительность в диапазоне [MinBookingDuration, MaxBookingDuration]. Разделяется
// между Booking.Create и Waitlist.Join, чтобы правила и их ошибки не расходились.
func validateInterval(now, start, end time.Time) error {
	if start.IsZero() || end.IsZero() {
		return &ValidationError{Field: "start_time", Message: "start_time and end_time are required"}
	}
	if !end.After(start) {
		return ErrInvalidTimeRange
	}
	if !start.After(now) {
		return ErrStartInPast
	}
	duration := end.Sub(start)
	if duration < MinBookingDuration {
		return ErrDurationTooShort
	}
	if duration > MaxBookingDuration {
		return ErrDurationTooLong
	}
	return nil
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

	title := strings.TrimSpace(in.Title)
	if title == "" {
		return model.Booking{}, &ValidationError{Field: "title", Message: "title is required"}
	}
	// Длину считаем в рунах: заголовки на русском, len() в байтах отверг бы
	// валидные строки короче лимита. Порог совпадает с maxLength в спеке.
	if utf8.RuneCountInString(title) > MaxTitleLength {
		return model.Booking{}, &ValidationError{Field: "title", Message: "title must be at most 200 characters"}
	}
	if strings.TrimSpace(in.RoomID) == "" {
		return model.Booking{}, &ValidationError{Field: "room_id", Message: "room_id is required"}
	}
	if err := validateInterval(s.now(), in.StartTime, in.EndTime); err != nil {
		return model.Booking{}, err
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

	// Большие переговорки (capacity > порога) требуют одобрения администратора: бронь
	// создаётся на согласовании и удерживает слот (см. предикат занятости репозитория),
	// а событие — booking.pending_approval вместо booking.created. Малые комнаты — как
	// прежде: сразу confirmed.
	status := model.StatusConfirmed
	eventType := events.TypeBookingCreated
	if room.Capacity > LargeRoomCapacityThreshold {
		status = model.StatusPendingApproval
		eventType = events.TypeBookingPendingApproval
	}

	b := model.Booking{
		ID:        "b-" + uuid.NewString(),
		RoomID:    in.RoomID,
		UserID:    a.ID,
		Title:     title,
		StartTime: in.StartTime.UTC(),
		EndTime:   in.EndTime.UTC(),
		Status:    status,
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

	if status == model.StatusPendingApproval {
		log.Info("booking created, pending approval")
	} else {
		log.Info("booking created")
	}

	// Комната занята на новом интервале — список свободных комнат в затронутых
	// окнах устарел, сбрасываем кэш доступности.
	s.invalidateRoomCache(ctx, log, b.RoomID)

	// Бронь зафиксирована — публикуем событие. Сбой публикации не откатывает
	// операцию (eventual consistency), только логируется.
	s.publishEvent(ctx, log, eventType, b)
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

	// Отмена и предложение освободившегося слота листу ожидания — в одной
	// транзакции, чтобы слот не был перехвачен между отменой и предложением.
	cancelled, offered, err := s.bookings.CancelAndOfferWaitlist(ctx, id, s.now())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// гонка: уже отменили между Get и Cancel
			return model.Booking{}, ErrAlreadyCancelled
		}
		log.Error("cancel booking: persist", slog.Any("error", err))
		return model.Booking{}, err
	}
	b = cancelled

	log.Info("booking cancelled")

	if offered != nil {
		log.Info("waitlist slot offered",
			"waitlist_id", offered.ID,
			"offered_user_id", offered.UserID,
			"position", offered.Position,
		)
	}

	// Комната освободилась на интервале брони — сбрасываем кэш доступности.
	s.invalidateRoomCache(ctx, log, b.RoomID)

	// Бронь отменена в БД — публикуем событие (см. Create про eventual consistency).
	s.publishEvent(ctx, log, events.TypeBookingCancelled, b)
	return b, nil
}

// ForceCancel — принудительная отмена брони администратором. От обычного Cancel
// отличается двумя правилами: (1) доступно только admin (не владельцу и не
// менеджеру этажа); (2) не применяет дедлайн CancelDeadline — админ вправе отменить
// бронь в любой момент, в том числе уже начавшуюся. Всё остальное идентично Cancel:
// та же атомарная отмена с предложением слота листу ожидания, сброс кэша доступности
// и публикация события booking.cancelled.
func (s *Booking) ForceCancel(ctx context.Context, a Actor, id string) (model.Booking, error) {
	// room_id добавляется ниже, когда бронь прочитана из БД.
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "booking_id", id)

	// Права проверяем до похода в БД: правило чисто ролевое (не зависит от брони),
	// поэтому не раскрываем существование чужой брони не-админу и не читаем зря.
	if !a.IsAdmin() {
		return model.Booking{}, ErrForbidden
	}

	b, err := s.bookings.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrBookingNotFound
		}
		log.Error("force cancel booking: lookup", slog.Any("error", err))
		return model.Booking{}, err
	}
	log = log.With("room_id", b.RoomID)

	if b.Status == model.StatusCancelled {
		return model.Booking{}, ErrAlreadyCancelled
	}

	// Дедлайн CancelDeadline намеренно НЕ проверяется — в этом и смысл принудительной
	// отмены. Отмена и предложение освободившегося слота листу ожидания — в одной
	// транзакции, чтобы слот не был перехвачен между отменой и предложением.
	cancelled, offered, err := s.bookings.CancelAndOfferWaitlist(ctx, id, s.now())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// гонка: уже отменили между Get и Cancel
			return model.Booking{}, ErrAlreadyCancelled
		}
		log.Error("force cancel booking: persist", slog.Any("error", err))
		return model.Booking{}, err
	}
	b = cancelled

	log.Info("booking force-cancelled")

	if offered != nil {
		log.Info("waitlist slot offered",
			"waitlist_id", offered.ID,
			"offered_user_id", offered.UserID,
			"position", offered.Position,
		)
	}

	// Комната освободилась на интервале брони — сбрасываем кэш доступности.
	s.invalidateRoomCache(ctx, log, b.RoomID)

	// Бронь отменена в БД — публикуем событие (см. Create про eventual consistency).
	s.publishEvent(ctx, log, events.TypeBookingCancelled, b)
	return b, nil
}

// ListPendingApprovals возвращает брони, ожидающие одобрения (только admin). Перед
// выдачей лениво авто-отклоняет просроченные (старше ApprovalTimeout): их слоты
// освобождаются и предлагаются очереди в репозитории, а сервис публикует для каждой
// событие booking.rejected. Не-admin → ErrForbidden.
func (s *Booking) ListPendingApprovals(ctx context.Context, a Actor) ([]model.Booking, error) {
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID)

	if !a.IsAdmin() {
		return nil, ErrForbidden
	}

	pending, autoRejected, err := s.bookings.ListPendingApprovals(ctx, s.now(), ApprovalTimeout, ApprovalTimeoutReason)
	if err != nil {
		log.Error("list pending approvals", slog.Any("error", err))
		return nil, err
	}

	for _, b := range autoRejected {
		blog := log.With("booking_id", b.ID, "room_id", b.RoomID)
		blog.Info("booking auto-rejected on approval timeout")
		// Слот освободился — сбрасываем кэш доступности и публикуем событие.
		s.invalidateRoomCache(ctx, blog, b.RoomID)
		publishBookingEvent(ctx, s.publisher, s.topic, blog, events.TypeBookingRejected, b, s.now())
	}
	return pending, nil
}

// Approve — одобрение брони большой комнаты администратором: переводит её из
// pending_approval в approved (слот остаётся занят). Доступно только admin. Просроченную
// (старше ApprovalTimeout) бронь одобрить нельзя — она лениво авто-отклоняется здесь же
// (rejected, слот освобождается и предлагается очереди, событие booking.rejected), а
// запрос получает ErrNotPendingApproval. Идемпотентность/гонка: повторное или
// конкурентное одобрение не-pending брони → ErrNotPendingApproval.
func (s *Booking) Approve(ctx context.Context, a Actor, id string) (model.Booking, error) {
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "booking_id", id)

	// Права проверяем до БД: правило чисто ролевое (как ForceCancel), чужую бронь
	// не-админу не раскрываем и зря в БД не ходим.
	if !a.IsAdmin() {
		return model.Booking{}, ErrForbidden
	}

	b, err := s.bookings.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrApprovalNotFound
		}
		log.Error("approve booking: lookup", slog.Any("error", err))
		return model.Booking{}, err
	}
	log = log.With("room_id", b.RoomID)

	if b.Status != model.StatusPendingApproval {
		return model.Booking{}, ErrNotPendingApproval
	}

	// Ленивый таймаут (воркера нет, протухание фиксируется при обращении). Просроченную
	// бронь авто-отклоняем со своей причиной; одобрить её уже нельзя.
	if s.now().Sub(b.CreatedAt) > ApprovalTimeout {
		rejected, offered, rErr := s.bookings.RejectAndOfferWaitlist(ctx, id, ApprovalTimeoutReason, s.now())
		if rErr != nil {
			if errors.Is(rErr, repository.ErrNotFound) {
				return model.Booking{}, ErrNotPendingApproval // гонка: уже не pending
			}
			log.Error("approve booking: auto-reject", slog.Any("error", rErr))
			return model.Booking{}, rErr
		}
		log.Info("booking auto-rejected on approval timeout")
		s.invalidateRoomCache(ctx, log, rejected.RoomID)
		logOffered(log, offered)
		s.publishEvent(ctx, log, events.TypeBookingRejected, rejected)
		return model.Booking{}, ErrNotPendingApproval
	}

	approved, err := s.bookings.Approve(ctx, id, s.now())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrNotPendingApproval // гонка: уже не pending
		}
		log.Error("approve booking: persist", slog.Any("error", err))
		return model.Booking{}, err
	}

	log.Info("booking approved")
	// Кэш доступности не трогаем: слот и так был занят (pending_approval → approved).
	s.publishEvent(ctx, log, events.TypeBookingApproved, approved)
	return approved, nil
}

// Reject — отклонение брони большой комнаты администратором с указанием причины:
// переводит её из pending_approval в rejected, сохраняет причину, освобождает слот и
// предлагает его листу ожидания (как отмена). Доступно только admin; reason обязателен.
// Идемпотентность/гонка: reject не-pending брони → ErrNotPendingApproval.
func (s *Booking) Reject(ctx context.Context, a Actor, id, reason string) (model.Booking, error) {
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "booking_id", id)

	if !a.IsAdmin() {
		return model.Booking{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return model.Booking{}, &ValidationError{Field: "reason", Message: "reason is required"}
	}

	b, err := s.bookings.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrApprovalNotFound
		}
		log.Error("reject booking: lookup", slog.Any("error", err))
		return model.Booking{}, err
	}
	log = log.With("room_id", b.RoomID)

	if b.Status != model.StatusPendingApproval {
		return model.Booking{}, ErrNotPendingApproval
	}

	rejected, offered, err := s.bookings.RejectAndOfferWaitlist(ctx, id, reason, s.now())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Booking{}, ErrNotPendingApproval // гонка: уже не pending
		}
		log.Error("reject booking: persist", slog.Any("error", err))
		return model.Booking{}, err
	}

	log.Info("booking rejected")
	// Слот освободился — сбрасываем кэш доступности.
	s.invalidateRoomCache(ctx, log, rejected.RoomID)
	logOffered(log, offered)
	s.publishEvent(ctx, log, events.TypeBookingRejected, rejected)
	return rejected, nil
}

// logOffered логирует waitlist-запись, которой предложили освободившийся слот (nil —
// no-op). Общий формат для reject и авто-reject по таймауту.
func logOffered(log *slog.Logger, offered *model.WaitlistEntry) {
	if offered != nil {
		log.Info("waitlist slot offered",
			"waitlist_id", offered.ID,
			"offered_user_id", offered.UserID,
			"position", offered.Position,
		)
	}
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
	invalidateRoomCache(ctx, s.cache, log, roomID)
}

// publishEvent публикует событие о брони после успешной записи в БД. Сбой
// публикации — деградация, а не провал операции: бронь уже зафиксирована,
// откатывать её из-за недоступной шины нельзя, поэтому уровень Warn. При
// nil-паблишере метод — no-op. log уже несёт контекст операции.
func (s *Booking) publishEvent(ctx context.Context, log *slog.Logger, eventType string, b model.Booking) {
	publishBookingEvent(ctx, s.publisher, s.topic, log, eventType, b, s.now())
}

// invalidateRoomCache — общий хелпер сброса кэша доступности комнаты (см. описание
// у Booking.invalidateRoomCache). Разделяется сервисами Booking и Waitlist.
func invalidateRoomCache(ctx context.Context, c cache.RoomCacheInterface, log *slog.Logger, roomID string) {
	if c == nil {
		return
	}
	if err := c.InvalidateRoomCache(ctx, roomID); err != nil {
		log.Warn("invalidate room cache", slog.Any("error", err))
	}
}

// publishBookingEvent — общий хелпер публикации события о брони (см. описание у
// Booking.publishEvent). Разделяется сервисами Booking и Waitlist.
func publishBookingEvent(ctx context.Context, p events.EventPublisher, topic string, log *slog.Logger, eventType string, b model.Booking, now time.Time) {
	if p == nil {
		return
	}
	ev := events.Event{
		EventID:   "evt-" + uuid.NewString(),
		Type:      eventType,
		BookingID: b.ID,
		UserID:    b.UserID,
		RoomID:    b.RoomID,
		Timestamp: now,
	}
	if err := p.Publish(ctx, topic, ev); err != nil {
		log.Warn("publish booking event, kafka unavailable",
			slog.Any("error", err),
			"type", eventType,
			"topic", topic,
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
