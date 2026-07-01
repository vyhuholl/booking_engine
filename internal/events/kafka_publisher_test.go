package events_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
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

// Интеграционные тесты Kafka-продюсера: реальный BookingService поверх Postgres
// пишет события в реальную Kafka, консьюмер вычитывает их из топика. Нужен Docker.
//
// TestMain поднимает контейнеры набора (Postgres — репозитории сервиса, Kafka —
// шина) один раз до старта тестов; sync.Once в testutil делает старт
// идемпотентным, так что тесты переиспользуют те же контейнеры, а Ryuk-reaper
// снимает их при выходе процесса. Если Docker недоступен, интеграционные тесты
// пропускаются (requireContainers), а Docker-free тесты пакета выполняются.

const integrationTraceparent = "00-1a2b3c4d5e6f70819200a1b2c3d4e5f6-00f067aa0ba902b7-01"

var integrationContainersErr error

func TestMain(m *testing.M) {
	if _, err := testutil.StartPostgres(); err != nil {
		integrationContainersErr = err
	} else if _, err := testutil.StartKafka(); err != nil {
		integrationContainersErr = err
	}
	os.Exit(m.Run())
}

func requireContainers(t *testing.T) {
	t.Helper()
	if integrationContainersErr != nil {
		t.Skipf("integration containers unavailable (Docker required): %v", integrationContainersErr)
	}
}

func TestKafkaPublisher_CreateBooking_EmitsBookingCreated(t *testing.T) {
	requireContainers(t)
	pool, cleanup := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)
	roomID, userID := testutil.FreshRoomAndUser(t, pool, cleanup)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(repository.NewRoom(pool), repository.NewBooking(pool), nil, publisher, topic, logger)

	ctx := tracing.WithTraceparent(context.Background(), integrationTraceparent)
	actor := service.Actor{ID: userID, Role: model.RoleMember}
	start := time.Now().Add(2 * time.Hour)
	booking, err := svc.Create(ctx, actor, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Standup",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)

	ev, msg := readOneEvent(t, brokers, topic)
	assert.Equal(t, events.TypeBookingCreated, ev.Type)
	assert.Equal(t, booking.ID, ev.BookingID)
	assert.Equal(t, userID, ev.UserID)
	assert.Equal(t, roomID, ev.RoomID)
	assert.WithinDuration(t, time.Now(), ev.Timestamp, time.Minute)

	// Ключ сообщения — BookingID (порядок событий одной брони), а trace-контекст
	// доехал в заголовке, а не в теле события.
	assert.Equal(t, []byte(booking.ID), msg.Key)
	assert.Equal(t, integrationTraceparent, headerValue(msg, tracing.HeaderName))
}

func TestKafkaPublisher_CancelBooking_EmitsBookingCancelled(t *testing.T) {
	requireContainers(t)
	pool, cleanup := testutil.SetupTestDB(t)
	brokers := testutil.SetupTestKafka(t)
	roomID, userID := testutil.FreshRoomAndUser(t, pool, cleanup)

	// Бронь сеем напрямую (в обход Create), чтобы в топик попало только событие
	// отмены. Начало — за 2 часа, дальше дедлайна отмены (30 минут).
	start := time.Now().Add(2 * time.Hour)
	seeded := testutil.Booking(
		testutil.WithBookingID("b-"+uuid.NewString()),
		testutil.WithRoom(roomID),
		testutil.WithOwner(userID),
		testutil.WithInterval(start, start.Add(time.Hour)),
	)
	testutil.SeedBooking(t, pool, seeded)

	topic := "booking.events." + uuid.NewString()
	createTopic(t, brokers, topic)

	logger, _ := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher(brokers, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	svc := service.NewBooking(repository.NewRoom(pool), repository.NewBooking(pool), nil, publisher, topic, logger)

	actor := service.Actor{ID: userID, Role: model.RoleMember}
	cancelled, err := svc.Cancel(context.Background(), actor, seeded.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusCancelled, cancelled.Status)

	ev, msg := readOneEvent(t, brokers, topic)
	assert.Equal(t, events.TypeBookingCancelled, ev.Type)
	assert.Equal(t, seeded.ID, ev.BookingID)
	assert.Equal(t, userID, ev.UserID)
	assert.Equal(t, roomID, ev.RoomID)
	assert.Equal(t, []byte(seeded.ID), msg.Key)
}

// TestKafkaPublisher_KafkaDown_BookingStillCreated фиксирует деградацию: при
// недоступной Kafka бронь всё равно создаётся (eventual consistency), а сбой
// публикации только логируется на уровне Warn.
func TestKafkaPublisher_KafkaDown_BookingStillCreated(t *testing.T) {
	requireContainers(t)
	pool, cleanup := testutil.SetupTestDB(t)
	roomID, userID := testutil.FreshRoomAndUser(t, pool, cleanup)

	// Паблишер смотрит на несуществующий брокер — публикация обречена.
	logger, logs := testutil.CaptureLogger()
	publisher := events.NewKafkaPublisher([]string{"127.0.0.1:1"}, logger)
	t.Cleanup(func() { _ = publisher.Close() })

	bookings := repository.NewBooking(pool)
	svc := service.NewBooking(repository.NewRoom(pool), bookings, nil, publisher, "booking.events", logger)

	// Короткий дедлайн: WriteMessages в мёртвый брокер упрётся в него, а не будет
	// висеть — при этом бронь фиксируется в БД до публикации, так что отмена
	// публикации по дедлайну её не откатывает.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	actor := service.Actor{ID: userID, Role: model.RoleMember}
	start := time.Now().Add(2 * time.Hour)
	booking, err := svc.Create(ctx, actor, service.BookingCreateInput{
		RoomID:    roomID,
		Title:     "Standup",
		StartTime: start,
		EndTime:   start.Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotEmpty(t, booking.ID)

	// Бронь действительно в БД и подтверждена.
	stored, err := bookings.Get(context.Background(), booking.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StatusConfirmed, stored.Status)

	// Сбой публикации залогирован как деградация Kafka.
	assert.Contains(t, logs.String(), "publish booking event, kafka unavailable")
}

// createTopic заранее создаёт топик с одной партицией: продюсер бьёт auto-create,
// но фиксированное число партиций делает чтение с партиции 0 детерминированным.
func createTopic(t *testing.T, brokers []string, topic string) {
	t.Helper()
	conn, err := kafka.Dial("tcp", brokers[0])
	require.NoError(t, err)
	defer conn.Close()

	controller, err := conn.Controller()
	require.NoError(t, err)
	controllerConn, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	require.NoError(t, err)
	defer controllerConn.Close()

	require.NoError(t, controllerConn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	}))
}

// readOneEvent вычитывает одно событие с партиции 0 топика (с начала лога) и
// возвращает его вместе с сырым сообщением — для проверок ключа и заголовков.
func readOneEvent(t *testing.T, brokers []string, topic string) (events.Event, kafka.Message) {
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
	require.NoError(t, r.SetOffset(kafka.FirstOffset))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg, err := r.ReadMessage(ctx)
	require.NoError(t, err)

	var ev events.Event
	require.NoError(t, json.Unmarshal(msg.Value, &ev))
	return ev, msg
}

func headerValue(msg kafka.Message, key string) string {
	for _, h := range msg.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
