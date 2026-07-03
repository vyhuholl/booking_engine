package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/service"
	"github.com/example/booking-engine/internal/testutil"
	"github.com/example/booking-engine/internal/tracing"
)

// Интеграционные тесты approval workflow: реальный BookingService поверх Postgres
// пишет события в реальную Kafka, консьюмер вычитывает их из топика. Нужен Docker.
//
// TestMain (в kafka_publisher_test.go) поднимает контейнеры (Postgres, Kafka) один раз
// до старта тестов; тесты переиспользуют те же контейнеры.

// TestApprovalWorkflow_FullCycle проверяет полный цикл одобрения:
// 1. Создать бронирование на комнату с вместимостью 20 → статус pending_approval
// 2. Проверить, что событие booking.pending_approval опубликовано в Kafka
// 3. Вызвать approve → статус approved
// 4. Проверить, что событие booking.approved опубликовано
// 5. Попробовать approve повторно → ошибка ErrNotPendingApproval
func TestApprovalWorkflow_FullCycle(t *testing.T) {
	requireContainers(t)
	pool, _ := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)

	// Создаём комнату вместимостью 20 (больше LargeRoomCapacityThreshold = 12)
	roomID := testutil.SeedRoom(t, pool,
		testutil.WithCapacity(20),
		testutil.WithRoomStatus(model.RoomStatusActive),
	)

	// Создаём member и admin
	memberID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
	)
	adminID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleAdmin),
	)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(
		repository.NewRoom(pool),
		repository.NewBooking(pool),
		nil,
		publisher,
		topic,
		logger,
	)

	ctx := context.Background()
	start := time.Now().Add(2 * time.Hour)
	member := service.Actor{ID: memberID, Role: model.RoleMember}
	admin := service.Actor{ID: adminID, Role: model.RoleAdmin}

	var eventOffset int64 = 0 // Смещение для последовательного чтения событий

	// === Шаг 1: Создаём бронирование на большую комнату ===
	booking, err := svc.Create(ctx, member, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Big meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)

	// Статус должен быть pending_approval
	assert.Equal(t, model.StatusPendingApproval, booking.Status)

	// === Шаг 2: Проверяем событие booking.pending_approval ===
	ev, msg := readNextEvent(t, brokers, topic, &eventOffset)
	assert.Equal(t, events.TypeBookingPendingApproval, ev.Type)
	assert.Equal(t, booking.ID, ev.BookingID)
	assert.Equal(t, memberID, ev.UserID)
	assert.Equal(t, roomID, ev.RoomID)
	assert.Equal(t, []byte(booking.ID), msg.Key)

	// === Шаг 3: Admin одобряет бронирование ===
	approved, err := svc.Approve(ctx, admin, booking.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusApproved, approved.Status)
	assert.Equal(t, booking.ID, approved.ID)

	// === Шаг 4: Проверяем событие booking.approved ===
	ev, msg = readNextEvent(t, brokers, topic, &eventOffset)
	assert.Equal(t, events.TypeBookingApproved, ev.Type)
	assert.Equal(t, booking.ID, ev.BookingID)
	assert.Equal(t, []byte(booking.ID), msg.Key)

	// === Шаг 5: Повторный approve даёт ошибку ===
	_, err = svc.Approve(ctx, admin, booking.ID)
	assert.Error(t, err)
	assert.ErrorIs(t, err, service.ErrNotPendingApproval)
}

// TestApprovalWorkflow_ConflictOnPending проверяет:
// 1. Создать pending_approval бронирование
// 2. Попробовать забронировать тот же слот другим пользователем → конфликт
// 3. Reject первое бронирование → слот свободен
// 4. Забронировать тот же слот другим пользователем → успех
func TestApprovalWorkflow_ConflictOnPending(t *testing.T) {
	requireContainers(t)
	pool, _ := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)

	// Комната вместимостью 20
	roomID := testutil.SeedRoom(t, pool,
		testutil.WithCapacity(20),
		testutil.WithRoomStatus(model.RoomStatusActive),
	)

	// Два пользователя и admin
	user1ID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
		testutil.WithUserID("user-1"),
		testutil.WithEmail("user1@example.com"),
	)
	user2ID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
		testutil.WithUserID("user-2"),
		testutil.WithEmail("user2@example.com"),
	)
	adminID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleAdmin),
	)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(
		repository.NewRoom(pool),
		repository.NewBooking(pool),
		nil,
		publisher,
		topic,
		logger,
	)

	ctx := context.Background()
	start := time.Now().Add(2 * time.Hour)
	user1 := service.Actor{ID: user1ID, Role: model.RoleMember}
	user2 := service.Actor{ID: user2ID, Role: model.RoleMember}
	admin := service.Actor{ID: adminID, Role: model.RoleAdmin}

	var eventOffset int64 = 0 // Смещение для последовательного чтения событий

	// === Шаг 1: Создаём pending_approval бронирование ===
	booking1, err := svc.Create(ctx, user1, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Big meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, model.StatusPendingApproval, booking1.Status)

	// Проверяем событие booking.pending_approval
	ev, msg := readNextEvent(t, brokers, topic, &eventOffset)
	assert.Equal(t, events.TypeBookingPendingApproval, ev.Type)
	assert.Equal(t, booking1.ID, ev.BookingID)
	assert.Equal(t, []byte(booking1.ID), msg.Key)

	// === Шаг 2: Второй пользователь пытается забронировать тот же слот → конфликт ===
	_, err = svc.Create(ctx, user2, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Another meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	assert.Error(t, err)

	var conflictErr *service.BookingConflictError
	assert.ErrorAs(t, err, &conflictErr)
	assert.Equal(t, booking1.ID, conflictErr.ConflictingID)

	// === Шаг 3: Admin отклоняет первое бронирование ===
	rejectionReason := "Room double-booked"
	rejected, err := svc.Reject(ctx, admin, booking1.ID, rejectionReason)
	require.NoError(t, err)
	assert.Equal(t, model.StatusRejected, rejected.Status)
	assert.NotNil(t, rejected.RejectionReason)
	assert.Equal(t, rejectionReason, *rejected.RejectionReason)

	// Проверяем событие booking.rejected
	ev, msg = readNextEvent(t, brokers, topic, &eventOffset)
	assert.Equal(t, events.TypeBookingRejected, ev.Type)
	assert.Equal(t, booking1.ID, ev.BookingID)
	assert.Equal(t, []byte(booking1.ID), msg.Key)

	// === Шаг 4: Второй пользователь может теперь забронировать тот же слот ===
	booking2, err := svc.Create(ctx, user2, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Another meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, model.StatusPendingApproval, booking2.Status)
	assert.NotEqual(t, booking1.ID, booking2.ID)

	// Проверяем новое событие booking.pending_approval для второй брони
	ev, msg = readNextEvent(t, brokers, topic, &eventOffset)
	assert.Equal(t, events.TypeBookingPendingApproval, ev.Type)
	assert.Equal(t, booking2.ID, ev.BookingID)
	assert.Equal(t, user2ID, ev.UserID)
	assert.Equal(t, []byte(booking2.ID), msg.Key)
}

// TestApprovalWorkflow_SmallRoomNoApproval проверяет, что маленькая комната
// (вместимость <= LargeRoomCapacityThreshold) создаётся в статусе confirmed
// и не требует одобрения.
func TestApprovalWorkflow_SmallRoomNoApproval(t *testing.T) {
	requireContainers(t)
	pool, _ := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)

	// Комната вместимостью 6 (не требует одобрения)
	roomID := testutil.SeedRoom(t, pool,
		testutil.WithCapacity(6),
		testutil.WithRoomStatus(model.RoomStatusActive),
	)

	userID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
	)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(
		repository.NewRoom(pool),
		repository.NewBooking(pool),
		nil,
		publisher,
		topic,
		logger,
	)

	ctx := context.Background()
	start := time.Now().Add(2 * time.Hour)
	user := service.Actor{ID: userID, Role: model.RoleMember}

	booking, err := svc.Create(ctx, user, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Small meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)

	// Статус должен быть confirmed, не pending_approval
	assert.Equal(t, model.StatusConfirmed, booking.Status)

	// Событие должно быть booking.created, не booking.pending_approval
	ev, msg := readOneEvent(t, brokers, topic)
	assert.Equal(t, events.TypeBookingCreated, ev.Type)
	assert.Equal(t, booking.ID, ev.BookingID)
	assert.Equal(t, []byte(booking.ID), msg.Key)
}

// TestApprovalWorkflow_NonAdminCannotApprove проверяет, что только admin
// может одобрять брони.
func TestApprovalWorkflow_NonAdminCannotApprove(t *testing.T) {
	requireContainers(t)
	pool, _ := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)

	roomID := testutil.SeedRoom(t, pool,
		testutil.WithCapacity(20),
		testutil.WithRoomStatus(model.RoomStatusActive),
	)

	userID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
	)
	adminID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleAdmin),
	)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(
		repository.NewRoom(pool),
		repository.NewBooking(pool),
		nil,
		publisher,
		topic,
		logger,
	)

	ctx := context.Background()
	start := time.Now().Add(2 * time.Hour)
	user := service.Actor{ID: userID, Role: model.RoleMember}

	// Создаём pending_approval бронь
	booking, err := svc.Create(ctx, user, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Big meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, model.StatusPendingApproval, booking.Status)

	// Пользователь не может одобрить свою же бронь
	_, err = svc.Approve(ctx, user, booking.ID)
	assert.Error(t, err)
	assert.ErrorIs(t, err, service.ErrForbidden)

	// Admin может одобрить
	admin := service.Actor{ID: adminID, Role: model.RoleAdmin}
	approved, err := svc.Approve(ctx, admin, booking.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusApproved, approved.Status)
}

// TestApprovalWorkflow_RejectRequiresReason проверяет, что reject
// обязательно требует причину.
func TestApprovalWorkflow_RejectRequiresReason(t *testing.T) {
	requireContainers(t)
	pool, _ := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)

	roomID := testutil.SeedRoom(t, pool,
		testutil.WithCapacity(20),
		testutil.WithRoomStatus(model.RoomStatusActive),
	)

	userID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
	)
	adminID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleAdmin),
	)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(
		repository.NewRoom(pool),
		repository.NewBooking(pool),
		nil,
		publisher,
		topic,
		logger,
	)

	ctx := context.Background()
	start := time.Now().Add(2 * time.Hour)
	user := service.Actor{ID: userID, Role: model.RoleMember}
	admin := service.Actor{ID: adminID, Role: model.RoleAdmin}

	booking, err := svc.Create(ctx, user, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Big meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)

	// Пустая причина — ошибка валидации
	_, err = svc.Reject(ctx, admin, booking.ID, "")
	assert.Error(t, err)
	var validationErr *service.ValidationError
	assert.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "reason", validationErr.Field)

	// Только пробелы — тоже ошибка
	_, err = svc.Reject(ctx, admin, booking.ID, "   ")
	assert.Error(t, err)

	// Нормальная причина — успех
	rejected, err := svc.Reject(ctx, admin, booking.ID, "Not appropriate")
	require.NoError(t, err)
	assert.Equal(t, model.StatusRejected, rejected.Status)
	assert.NotNil(t, rejected.RejectionReason)
	assert.Equal(t, "Not appropriate", *rejected.RejectionReason)
}

// TestApprovalWorkflow_TraceContext проверяет, что trace-контекст
// пробрасывается в заголовки Kafka-сообщений.
func TestApprovalWorkflow_TraceContext(t *testing.T) {
	requireContainers(t)
	pool, _ := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)

	roomID := testutil.SeedRoom(t, pool,
		testutil.WithCapacity(20),
		testutil.WithRoomStatus(model.RoomStatusActive),
	)

	userID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
	)
	adminID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleAdmin),
	)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(
		repository.NewRoom(pool),
		repository.NewBooking(pool),
		nil,
		publisher,
		topic,
		logger,
	)

	const testTraceparent = "00-1a2b3c4d5e6f70819200a1b2c3d4e5f6-00f067aa0ba902b7-01"

	ctx := tracing.WithTraceparent(context.Background(), testTraceparent)
	start := time.Now().Add(2 * time.Hour)
	user := service.Actor{ID: userID, Role: model.RoleMember}
	admin := service.Actor{ID: adminID, Role: model.RoleAdmin}

	var eventOffset int64 = 0 // Смещение для последовательного чтения событий

	// Создаём бронь с trace-контекстом
	booking, err := svc.Create(ctx, user, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Big meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)

	// Проверяем traceparent в заголовке первого события
	_, msg := readNextEvent(t, brokers, topic, &eventOffset)
	assert.Equal(t, testTraceparent, headerValue(msg, tracing.HeaderName))

	// Approve с тем же контекстом
	approved, err := svc.Approve(ctx, admin, booking.ID)
	require.NoError(t, err)

	// Проверяем traceparent в заголовке второго события
	_, msg = readNextEvent(t, brokers, topic, &eventOffset)
	assert.Equal(t, testTraceparent, headerValue(msg, tracing.HeaderName))
	assert.Equal(t, model.StatusApproved, approved.Status)
}

// readNextEvent вычитывает следующее событие из топика (последовательно).
// Использует смещение, хранящееся в контексте теста, через тестовое состояние.
func readNextEvent(t *testing.T, brokers []string, topic string, offset *int64) (events.Event, kafka.Message) {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   brokers,
		Topic:     topic,
		Partition: 0,
		MinBytes:  1,
		MaxBytes:  10 << 20,
		MaxWait:   250 * time.Millisecond,
	})
	defer r.Close()

	// Устанавливаем оффсет. Если это первый вызов (*offset == 0), читаем с FirstOffset
	if *offset == 0 {
		require.NoError(t, r.SetOffset(kafka.FirstOffset))
	} else {
		require.NoError(t, r.SetOffset(*offset))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := r.ReadMessage(ctx)
	require.NoError(t, err)

	// Сохраняем следующий оффсет
	*offset = msg.Offset + 1

	var ev events.Event
	require.NoError(t, json.Unmarshal(msg.Value, &ev))
	return ev, msg
}

// TestApprovalWorkflow_Idempotency проверяет идемпотентность approve:
// повторный approve даёт ErrNotPendingApproval, статус в БД не меняется.
func TestApprovalWorkflow_Idempotency(t *testing.T) {
	requireContainers(t)
	pool, _ := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)

	roomID := testutil.SeedRoom(t, pool,
		testutil.WithCapacity(20),
		testutil.WithRoomStatus(model.RoomStatusActive),
	)

	userID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleMember),
	)
	adminID := testutil.SeedUser(t, pool,
		testutil.WithRole(model.RoleAdmin),
	)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(
		repository.NewRoom(pool),
		repository.NewBooking(pool),
		nil,
		publisher,
		topic,
		logger,
	)

	ctx := context.Background()
	start := time.Now().Add(2 * time.Hour)
	user := service.Actor{ID: userID, Role: model.RoleMember}
	admin := service.Actor{ID: adminID, Role: model.RoleAdmin}

	booking, err := svc.Create(ctx, user, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Big meeting",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)

	// Первый approve — успех
	approved, err := svc.Approve(ctx, admin, booking.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusApproved, approved.Status)

	// Второй approve — ошибка (уже не pending)
	_, err = svc.Approve(ctx, admin, booking.ID)
	assert.Error(t, err)
	assert.ErrorIs(t, err, service.ErrNotPendingApproval)

	// В БД статус должен остаться approved (не изменился при втором вызове)
	repo := repository.NewBooking(pool)
	stored, err := repo.Get(ctx, booking.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusApproved, stored.Status)
}
