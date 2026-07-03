package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/booking-engine/internal/notifications"
	"github.com/example/booking-engine/internal/repository"
)

type config struct {
	DatabaseURL  string
	KafkaBrokers []string
	KafkaTopic   string
	KafkaGroupID string
	RetryMax     int
	RetryBaseSec int
}

func loadConfig() (config, error) {
	cfg := config{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		KafkaBrokers: splitAndTrim(os.Getenv("KAFKA_BROKERS")),
		KafkaTopic:   getenv("KAFKA_TOPIC", "booking.events"),
		KafkaGroupID: getenv("KAFKA_GROUP_ID", "notifier"),
		RetryMax:     atoi(getenv("NOTIFY_RETRY_MAX", "3")),
		RetryBaseSec: atoi(getenv("NOTIFY_RETRY_BASE_SEC", "1")),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if len(cfg.KafkaBrokers) == 0 {
		return cfg, errors.New("KAFKA_BROKERS is required")
	}
	if cfg.RetryMax < 1 {
		cfg.RetryMax = 1
	}
	return cfg, nil
}

func splitAndTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	i, _ := strconv.Atoi(s)
	return i
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

	users := repository.NewUser(pool)
	bookings := repository.NewBooking(pool)
	notifyRepo := repository.NewNotification(pool)

	notifier := notifications.NewLogNotifier(logger)
	dispatcher := notifications.NewDispatcher(
		notifier,
		users,      // AdminLookup (ListAdmins)
		notifyRepo, // DedupStore
		notifyRepo, // DeadLetterStore
		notifications.Config{
			RetryMax:  cfg.RetryMax,
			RetryBase: time.Duration(cfg.RetryBaseSec) * time.Second,
		},
		logger,
		notifications.WithBookingLookup(bookings), // обогащение rejected reason
	)

	consumer := notifications.NewConsumer(cfg.KafkaBrokers, cfg.KafkaTopic, cfg.KafkaGroupID, dispatcher, logger)
	logger.Info("notifier started", "brokers", cfg.KafkaBrokers, "topic", cfg.KafkaTopic, "group", cfg.KafkaGroupID)

	// Run блокируется до rootCtx.Done() (SIGTERM/SIGINT) или ошибки чтения.
	if runErr := consumer.Run(rootCtx); runErr != nil {
		logger.Error("consumer run", "err", runErr)
		cancel()
	}

	if err := consumer.Close(); err != nil {
		logger.Error("consumer close", "err", err)
	}
	logger.Info("notifier shutdown")
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
