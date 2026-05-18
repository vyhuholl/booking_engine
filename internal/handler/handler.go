package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/service"
)

type Handler struct {
	log       *slog.Logger
	jwtSecret []byte
	users     UserLookup
	rooms     *service.Room
	bookings  *service.Booking
	usersvc   *service.User
}

type UserLookup interface {
	Get(ctx context.Context, id string) (model.User, error)
}

func New(
	log *slog.Logger,
	jwtSecret string,
	users UserLookup,
	rooms *service.Room,
	bookings *service.Booking,
	usersvc *service.User,
) *Handler {
	return &Handler{
		log:       log,
		jwtSecret: []byte(jwtSecret),
		users:     users,
		rooms:     rooms,
		bookings:  bookings,
		usersvc:   usersvc,
	}
}

func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(slogRequestLogger(h.log))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Group(func(r chi.Router) {
		r.Use(h.authMiddleware)

		r.Route("/rooms", func(r chi.Router) {
			r.Get("/", h.listRooms)
			r.Post("/", h.createRoom)
			r.Get("/available", h.searchAvailableRooms)
			r.Get("/{id}", h.getRoom)
			r.Put("/{id}", h.updateRoom)
			r.Delete("/{id}", h.deleteRoom)
			r.Get("/{id}/bookings", h.listRoomBookings)
		})

		r.Route("/bookings", func(r chi.Router) {
			r.Post("/", h.createBooking)
			r.Post("/{id}/cancel", h.cancelBooking)
		})

		r.Get("/users/{id}/bookings", h.listUserBookings)
	})

	return r
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func slogRequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"req_id", middleware.GetReqID(r.Context()),
			)
		})
	}
}
