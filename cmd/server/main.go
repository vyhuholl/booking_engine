package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/booking-engine/internal/handler"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
)

type config struct {
	HTTPAddr    string
	DatabaseURL string
	JWTSecret   string
}

func loadConfig() (config, error) {
	cfg := config{
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return cfg, errors.New("JWT_SECRET is required")
	}
	return cfg, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(rootCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("pgxpool init", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pingWithRetry(rootCtx, logger, pool, 30, time.Second); err != nil {
		logger.Error("postgres unreachable", "err", err)
		os.Exit(1)
	}

	rooms := repository.NewRoom(pool)
	bookings := repository.NewBooking(pool)
	users := repository.NewUser(pool)

	roomSvc := service.NewRoom(rooms, bookings)
	bookingSvc := service.NewBooking(rooms, bookings)
	userSvc := service.NewUser(users)

	h := handler.New(logger, cfg.JWTSecret, users, roomSvc, bookingSvc, userSvc)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           h.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http serve", "err", err)
			cancel()
		}
	}()

	<-rootCtx.Done()
	logger.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}
}

func pingWithRetry(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, attempts int, delay time.Duration) error {
	var lastErr error
	for i := 1; i <= attempts; i++ {
		if err := pool.Ping(ctx); err == nil {
			return nil
		} else {
			lastErr = err
			log.Warn("postgres ping failed", "attempt", i, "err", err)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}
