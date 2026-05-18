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
)

type Room struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Capacity  int         `json:"capacity"`
	Floor     int         `json:"floor"`
	Equipment []Equipment `json:"equipment"`
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
}
