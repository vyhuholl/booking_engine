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

type RoomRepo interface {
	List(ctx context.Context, f repository.RoomFilter) ([]model.Room, int, error)
	Get(ctx context.Context, id string) (model.Room, error)
	Create(ctx context.Context, r model.Room) error
	Update(ctx context.Context, r model.Room) error
	Delete(ctx context.Context, id string) error
	Available(ctx context.Context, start, end string, capacityMin *int, floor *int, equipment []string) ([]model.Room, error)
}

type BookingsForRoom interface {
	HasActiveForRoom(ctx context.Context, roomID string, after time.Time) (bool, error)
	ListByRoomOnDate(ctx context.Context, roomID string, date time.Time) ([]model.Booking, error)
}

type Room struct {
	rooms    RoomRepo
	bookings BookingsForRoom
	now      func() time.Time
}

func NewRoom(rooms RoomRepo, bookings BookingsForRoom) *Room {
	return &Room{rooms: rooms, bookings: bookings, now: time.Now}
}

type RoomCreateInput struct {
	Name      string
	Capacity  int
	Floor     int
	Equipment []model.Equipment
}

type RoomUpdateInput struct {
	Name      *string
	Capacity  *int
	Floor     *int
	Equipment *[]model.Equipment
}

func (s *Room) List(ctx context.Context, _ Actor, f repository.RoomFilter) ([]model.Room, int, error) {
	return s.rooms.List(ctx, f)
}

func (s *Room) Get(ctx context.Context, _ Actor, id string) (model.Room, error) {
	return s.getOr404(ctx, id)
}

func (s *Room) Create(ctx context.Context, a Actor, in RoomCreateInput) (model.Room, error) {
	if !a.IsAdmin() {
		return model.Room{}, ErrForbidden
	}
	if err := validateRoom(in.Name, in.Capacity, in.Equipment); err != nil {
		return model.Room{}, err
	}
	r := model.Room{
		ID:        "r-" + uuid.NewString(),
		Name:      strings.TrimSpace(in.Name),
		Capacity:  in.Capacity,
		Floor:     in.Floor,
		Equipment: dedupEquipment(in.Equipment),
	}
	if err := s.rooms.Create(ctx, r); err != nil {
		return model.Room{}, err
	}
	return r, nil
}

func (s *Room) Update(ctx context.Context, a Actor, id string, in RoomUpdateInput) (model.Room, error) {
	if !a.IsAdmin() {
		return model.Room{}, ErrForbidden
	}
	cur, err := s.getOr404(ctx, id)
	if err != nil {
		return model.Room{}, err
	}
	if in.Name != nil {
		cur.Name = strings.TrimSpace(*in.Name)
	}
	if in.Capacity != nil {
		cur.Capacity = *in.Capacity
	}
	if in.Floor != nil {
		cur.Floor = *in.Floor
	}
	if in.Equipment != nil {
		cur.Equipment = dedupEquipment(*in.Equipment)
	}
	if err := validateRoom(cur.Name, cur.Capacity, cur.Equipment); err != nil {
		return model.Room{}, err
	}
	if err := s.rooms.Update(ctx, cur); err != nil {
		return model.Room{}, mapRoomNotFound(err)
	}
	return cur, nil
}

func (s *Room) Delete(ctx context.Context, a Actor, id string) error {
	if !a.IsAdmin() {
		return ErrForbidden
	}
	if _, err := s.getOr404(ctx, id); err != nil {
		return err
	}
	has, err := s.bookings.HasActiveForRoom(ctx, id, s.now())
	if err != nil {
		return err
	}
	if has {
		return ErrRoomHasActiveBookings
	}
	if err := s.rooms.Delete(ctx, id); err != nil {
		return mapRoomNotFound(err)
	}
	return nil
}

type AvailableQuery struct {
	Start       time.Time
	End         time.Time
	CapacityMin *int
	Floor       *int
	Equipment   []model.Equipment
}

func (s *Room) Available(ctx context.Context, _ Actor, q AvailableQuery) ([]model.Room, error) {
	if !q.End.After(q.Start) {
		return nil, ErrInvalidTimeRange
	}
	if q.CapacityMin != nil && *q.CapacityMin < 1 {
		return nil, &ValidationError{Field: "capacity_min", Message: "capacity_min must be >= 1"}
	}
	for _, e := range q.Equipment {
		if !e.Valid() {
			return nil, &ValidationError{Field: "equipment", Message: "unknown equipment: " + string(e)}
		}
	}
	eq := make([]string, 0, len(q.Equipment))
	for _, e := range q.Equipment {
		eq = append(eq, string(e))
	}
	return s.rooms.Available(ctx, q.Start.UTC().Format(time.RFC3339), q.End.UTC().Format(time.RFC3339), q.CapacityMin, q.Floor, eq)
}

func (s *Room) BookingsOnDate(ctx context.Context, _ Actor, roomID string, date time.Time) ([]model.Booking, error) {
	if _, err := s.getOr404(ctx, roomID); err != nil {
		return nil, err
	}
	return s.bookings.ListByRoomOnDate(ctx, roomID, date)
}

func (s *Room) getOr404(ctx context.Context, id string) (model.Room, error) {
	r, err := s.rooms.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return model.Room{}, ErrRoomNotFound
		}
		return model.Room{}, err
	}
	return r, nil
}

func mapRoomNotFound(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return ErrRoomNotFound
	}
	return err
}

func validateRoom(name string, capacity int, equipment []model.Equipment) error {
	if strings.TrimSpace(name) == "" {
		return &ValidationError{Field: "name", Message: "name is required"}
	}
	if capacity < 1 {
		return &ValidationError{Field: "capacity", Message: "capacity must be >= 1"}
	}
	for _, e := range equipment {
		if !e.Valid() {
			return &ValidationError{Field: "equipment", Message: "unknown equipment: " + string(e)}
		}
	}
	return nil
}

func dedupEquipment(in []model.Equipment) []model.Equipment {
	seen := map[model.Equipment]struct{}{}
	out := make([]model.Equipment, 0, len(in))
	for _, e := range in {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}
