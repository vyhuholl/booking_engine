package service

import "github.com/example/booking-engine/internal/model"

// Actor — пользователь, выполняющий операцию. Передаётся явно в сервисы,
// чтобы handler не вмешивался в авторизацию.
type Actor struct {
	ID           string
	Role         model.Role
	ManagesFloor *int
}

func ActorFromUser(u model.User) Actor {
	return Actor{ID: u.ID, Role: u.Role, ManagesFloor: u.ManagesFloor}
}

func (a Actor) IsAdmin() bool   { return a.Role == model.RoleAdmin }
func (a Actor) IsManager() bool { return a.Role == model.RoleManager }
