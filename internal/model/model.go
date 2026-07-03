package model

import "time"

type Equipment string

const (
	EquipmentProjector  Equipment = "projector"
	EquipmentWhiteboard Equipment = "whiteboard"
	EquipmentVideoConf  Equipment = "video_conf"
)

func (e Equipment) Valid() bool {
	switch e {
	case EquipmentProjector, EquipmentWhiteboard, EquipmentVideoConf:
		return true
	}
	return false
}

type Role string

const (
	RoleAdmin   Role = "admin"
	RoleManager Role = "manager"
	RoleMember  Role = "member"
)

type BookingStatus string

const (
	StatusConfirmed BookingStatus = "confirmed"
	StatusCancelled BookingStatus = "cancelled"

	// Статусы workflow одобрения больших переговорок (change add-large-room-approval):
	// бронь комнаты с capacity > 12 создаётся в pending_approval и удерживает слот,
	// admin переводит её в approved (слот остаётся занят) или rejected (слот освобождается).
	StatusPendingApproval BookingStatus = "pending_approval"
	StatusApproved        BookingStatus = "approved"
	StatusRejected        BookingStatus = "rejected"
)

// ActiveBookingStatuses — статусы, при которых бронь удерживает слот комнаты и
// потому учитывается в проверке пересечений (занятости). rejected и cancelled слот
// освобождают и сюда не входят. Единый источник правды для предикатов занятости в
// repository (CreateChecked/IsRoomBusy/ListConflicting) — чтобы они не разошлись.
var ActiveBookingStatuses = []BookingStatus{
	StatusConfirmed,
	StatusPendingApproval,
	StatusApproved,
}

// IsActive сообщает, удерживает ли бронь в этом статусе слот комнаты.
func (s BookingStatus) IsActive() bool {
	for _, a := range ActiveBookingStatuses {
		if s == a {
			return true
		}
	}
	return false
}

type RoomStatus string

const (
	RoomStatusActive       RoomStatus = "active"
	RoomStatusOutOfService RoomStatus = "out_of_service"
)

func (s RoomStatus) Valid() bool {
	switch s {
	case RoomStatusActive, RoomStatusOutOfService:
		return true
	}
	return false
}

// WaitlistStatus — жизненный цикл записи листа ожидания:
// waiting → offered → (converted | expired).
type WaitlistStatus string

const (
	WaitlistStatusWaiting   WaitlistStatus = "waiting"   // в очереди, ждёт освобождения слота
	WaitlistStatusOffered   WaitlistStatus = "offered"   // слот предложен, ждёт подтверждения
	WaitlistStatusExpired   WaitlistStatus = "expired"   // предложение протухло (не подтверждено за OfferTTL)
	WaitlistStatusConverted WaitlistStatus = "converted" // подтверждено, создана обычная бронь
)

func (s WaitlistStatus) Valid() bool {
	switch s {
	case WaitlistStatusWaiting, WaitlistStatusOffered, WaitlistStatusExpired, WaitlistStatusConverted:
		return true
	}
	return false
}

type Room struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Capacity  int         `json:"capacity"`
	Floor     int         `json:"floor"`
	Equipment []Equipment `json:"equipment"`
	Status    RoomStatus  `json:"status"`
}

type User struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Role         Role   `json:"role"`
	ManagesFloor *int   `json:"-"`
}

type Booking struct {
	ID        string        `json:"id"`
	RoomID    string        `json:"room_id"`
	UserID    string        `json:"user_id"`
	Title     string        `json:"title"`
	StartTime time.Time     `json:"start_time"`
	EndTime   time.Time     `json:"end_time"`
	Status    BookingStatus `json:"status"`
	// RejectionReason — причина отклонения (заполняется при reject/авто-reject
	// брони на согласовании), nil для остальных статусов.
	RejectionReason *string `json:"rejection_reason,omitempty"`
	// CreatedAt — момент создания брони, якорь 24-часового таймаута одобрения
	// (заполняется репозиторием из колонки created_at). Внутреннее поле — в JSON
	// не отдаётся.
	CreatedAt time.Time `json:"-"`
}

// WaitlistEntry — запись листа ожидания на занятый интервал комнаты. Position —
// автоназначаемый ординал в очереди комнаты (не редактируется клиентом). OfferedAt
// заполняется при переходе в статус offered, иначе nil.
type WaitlistEntry struct {
	ID        string         `json:"id"`
	RoomID    string         `json:"room_id"`
	UserID    string         `json:"user_id"`
	StartTime time.Time      `json:"start_time"`
	EndTime   time.Time      `json:"end_time"`
	Position  int            `json:"position"`
	Status    WaitlistStatus `json:"status"`
	OfferedAt *time.Time     `json:"offered_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// BookingFilters — необязательные фильтры выборки броней. nil-поле означает
// «без фильтра по этому измерению» (учитываются все значения).
type BookingFilters struct {
	Status *string `json:"status,omitempty"`  // фильтр по статусу брони, nil = любой
	UserID *string `json:"user_id,omitempty"` // фильтр по владельцу, nil = любой
}

// DailyBookings — брони одного дня, сгруппированные для недельной выборки.
type DailyBookings struct {
	Date     time.Time `json:"date"`     // начало суток в UTC
	Bookings []Booking `json:"bookings"` // брони этого дня (пустой список, не nil)
}
