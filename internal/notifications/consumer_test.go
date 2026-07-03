package notifications

// Тесты NotificationConsumer написаны ДО реализации (TDD, red): они фиксируют
// контракт консьюмера через публичный API пакета notifications, которого пока
// нет. Пока не появятся типы Notifier/Notification/Dispatcher/Consumer/Config и
// поле events.Event.EventID — пакет не компилируется, и все тесты «падают».
//
// Инфраструктура: реальная Kafka через testcontainers (как в
// internal/events/kafka_publisher_test.go), Notifier — ручной мок. Топик на
// каждый тест — с одной партицией, поэтому события обрабатываются строго в
// порядке публикации; это даёт детерминированные «барьерные» проверки.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/testutil"
)

// Значения типов событий в шине (контракт internal/events).
const (
	evCreated         = "booking.created"
	evPendingApproval = "booking.pending_approval"
	evUnknown         = "booking.teleported"
)

var integrationContainersErr error

func TestMain(m *testing.M) {
	if _, err := testutil.StartKafka(); err != nil {
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

// --- Сценарий 1: booking.created → уведомление владельца ------------------

func TestConsumer_BookingCreated_NotifiesOwner(t *testing.T) {
	requireContainers(t)
	brokers := testutil.SetupTestKafka(t)
	topic := newTopic(t, brokers)

	notifier := &spyNotifier{}
	disp := newDispatcher(notifier, stubAdmins{}, newMemDedup(), &memDeadLetter{})

	publish(t, brokers, topic, evt("evt-1", evCreated, "b-1", "user-owner"))
	runConsumer(t, brokers, topic, disp)

	require.Eventually(t, func() bool { return notifier.count() == 1 },
		30*time.Second, 50*time.Millisecond, "owner should be notified exactly once")

	got := notifier.snapshot()[0]
	assert.Equal(t, "user-owner", got.userID)
	assert.Equal(t, NotifyBookingConfirmed, got.n.Type)
	assert.Equal(t, "b-1", got.n.BookingID)
}

// --- Сценарий 2: booking.pending_approval → уведомление всех админов -------

func TestConsumer_PendingApproval_NotifiesAllAdmins(t *testing.T) {
	requireContainers(t)
	brokers := testutil.SetupTestKafka(t)
	topic := newTopic(t, brokers)

	admins := stubAdmins{admins: []model.User{
		{ID: "user-admin-1", Role: model.RoleAdmin},
		{ID: "user-admin-2", Role: model.RoleAdmin},
	}}
	notifier := &spyNotifier{}
	disp := newDispatcher(notifier, admins, newMemDedup(), &memDeadLetter{})

	publish(t, brokers, topic, evt("evt-2", evPendingApproval, "b-2", "user-owner"))
	runConsumer(t, brokers, topic, disp)

	require.Eventually(t, func() bool { return notifier.count() == 2 },
		30*time.Second, 50*time.Millisecond, "both admins should be notified")

	assert.Equal(t, map[string]bool{"user-admin-1": true, "user-admin-2": true}, notifier.recipients())
	for _, c := range notifier.snapshot() {
		assert.Equal(t, NotifyApprovalRequested, c.n.Type)
	}
}

// --- Сценарий 3: дубль события (тот же event_id) → уведомление один раз -----

func TestConsumer_DuplicateEvent_NotifiesOnce(t *testing.T) {
	requireContainers(t)
	brokers := testutil.SetupTestKafka(t)
	topic := newTopic(t, brokers)

	notifier := &spyNotifier{}
	disp := newDispatcher(notifier, stubAdmins{}, newMemDedup(), &memDeadLetter{})

	// Один и тот же event_id публикуется дважды, затем «барьер» для другого
	// пользователя. Топик односекционный → порядок гарантирован: увидев
	// уведомление барьера, знаем, что оба дубля уже обработаны.
	dup := evt("evt-dup", evCreated, "b-dup", "user-owner")
	barrier := evt("evt-barrier", evCreated, "b-barrier", "user-barrier")
	publish(t, brokers, topic, dup, dup, barrier)
	runConsumer(t, brokers, topic, disp)

	require.Eventually(t, func() bool { return notifier.countFor("user-barrier") == 1 },
		30*time.Second, 50*time.Millisecond, "barrier event should be processed")

	assert.Equal(t, 1, notifier.countFor("user-owner"),
		"duplicate event (same event_id) must notify owner exactly once")
}

// --- Сценарий 4: ошибка Notifier.Send → повторная попытка (retry) ----------

func TestConsumer_SendError_Retries(t *testing.T) {
	requireContainers(t)
	brokers := testutil.SetupTestKafka(t)
	topic := newTopic(t, brokers)

	// Первая попытка падает, вторая — успех.
	notifier := &spyNotifier{failFn: func(attempt int) error {
		if attempt == 1 {
			return errors.New("transient send failure")
		}
		return nil
	}}
	dl := &memDeadLetter{}
	disp := newDispatcher(notifier, stubAdmins{}, newMemDedup(), dl)

	publish(t, brokers, topic, evt("evt-4", evCreated, "b-4", "user-owner"))
	runConsumer(t, brokers, topic, disp)

	require.Eventually(t, func() bool { return notifier.count() >= 2 },
		30*time.Second, 50*time.Millisecond, "Send must be retried after failure")

	assert.Equal(t, 2, notifier.count(), "should retry once then succeed")
	assert.Equal(t, "user-owner", notifier.snapshot()[1].userID)
	assert.Empty(t, dl.snapshot(), "successful retry must not dead-letter")
}

// --- Сценарий 5: неизвестный тип события → пропуск без ошибки --------------

func TestConsumer_UnknownEventType_Skipped(t *testing.T) {
	requireContainers(t)
	brokers := testutil.SetupTestKafka(t)
	topic := newTopic(t, brokers)

	notifier := &spyNotifier{}
	disp := newDispatcher(notifier, stubAdmins{}, newMemDedup(), &memDeadLetter{})

	// Неизвестный тип, затем известный «барьер». Барьер обрабатывается строго
	// после неизвестного (односекционный топик) — значит консьюмер не упал на
	// неизвестном типе, а пропустил его без уведомлений.
	unknown := evt("evt-5", evUnknown, "b-5", "user-owner")
	barrier := evt("evt-5b", evCreated, "b-5b", "user-barrier")
	publish(t, brokers, topic, unknown, barrier)
	runConsumer(t, brokers, topic, disp)

	require.Eventually(t, func() bool { return notifier.countFor("user-barrier") == 1 },
		30*time.Second, 50*time.Millisecond, "consumer must keep processing after unknown type")

	assert.Equal(t, 0, notifier.countFor("user-owner"), "unknown event type must not notify")
	assert.Equal(t, 1, notifier.count(), "only the known barrier event should notify")
}

// --- Бонус: исчерпание попыток → dead-letter ------------------------------

func TestConsumer_SendExhausted_DeadLetters(t *testing.T) {
	requireContainers(t)
	brokers := testutil.SetupTestKafka(t)
	topic := newTopic(t, brokers)

	notifier := &spyNotifier{failFn: func(int) error { return errors.New("permanent failure") }}
	dl := &memDeadLetter{}
	disp := newDispatcher(notifier, stubAdmins{}, newMemDedup(), dl)

	publish(t, brokers, topic, evt("evt-6", evCreated, "b-6", "user-owner"))
	runConsumer(t, brokers, topic, disp)

	require.Eventually(t, func() bool { return len(dl.snapshot()) == 1 },
		30*time.Second, 50*time.Millisecond, "undelivered notification must be dead-lettered")

	assert.Equal(t, 3, notifier.count(), "must exhaust all retry attempts (RetryMax)")
	entry := dl.snapshot()[0]
	assert.Equal(t, "user-owner", entry.userID)
	assert.Equal(t, NotifyBookingConfirmed, entry.notificationType)
}

// --- Моки -----------------------------------------------------------------

// spyNotifier — потокобезопасный мок Notifier: записывает вызовы Send и может
// возвращать ошибку по номеру попытки (failFn). Вызов записывается всегда, даже
// при ошибке, чтобы проверять факт повторной попытки.
type spyNotifier struct {
	mu     sync.Mutex
	calls  []sentNotification
	failFn func(attempt int) error
}

type sentNotification struct {
	userID string
	n      Notification
}

func (s *spyNotifier) Send(_ context.Context, userID string, n Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := len(s.calls) + 1
	s.calls = append(s.calls, sentNotification{userID: userID, n: n})
	if s.failFn != nil {
		return s.failFn(attempt)
	}
	return nil
}

func (s *spyNotifier) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *spyNotifier) countFor(userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.userID == userID {
			n++
		}
	}
	return n
}

func (s *spyNotifier) snapshot() []sentNotification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentNotification(nil), s.calls...)
}

func (s *spyNotifier) recipients() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]bool, len(s.calls))
	for _, c := range s.calls {
		set[c.userID] = true
	}
	return set
}

// stubAdmins — мок AdminLookup: отдаёт заранее заданный список админов.
type stubAdmins struct {
	admins []model.User
	err    error
}

func (s stubAdmins) ListAdmins(_ context.Context) ([]model.User, error) {
	return s.admins, s.err
}

// memDedup — in-memory DedupStore для проверки идемпотентности (замена таблицы
// processed_events в тестах).
type memDedup struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newMemDedup() *memDedup { return &memDedup{seen: make(map[string]bool)} }

func (m *memDedup) IsProcessed(_ context.Context, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[eventID], nil
}

func (m *memDedup) MarkProcessed(_ context.Context, eventID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen[eventID] = true
	return nil
}

// memDeadLetter — in-memory DeadLetterStore (замена notification_dead_letter).
type deadLetterEntry struct {
	eventID          string
	userID           string
	notificationType string
	reason           string
}

type memDeadLetter struct {
	mu      sync.Mutex
	entries []deadLetterEntry
}

func (m *memDeadLetter) SaveDeadLetter(_ context.Context, eventID, userID, notificationType, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, deadLetterEntry{eventID, userID, notificationType, reason})
	return nil
}

func (m *memDeadLetter) snapshot() []deadLetterEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]deadLetterEntry(nil), m.entries...)
}

// --- Хелперы --------------------------------------------------------------

// newDispatcher собирает Dispatcher с мелким backoff, чтобы ретраи не замедляли
// тесты. RetryMax=3 — на нём проверяется исчерпание попыток.
func newDispatcher(n Notifier, admins AdminLookup, dedup DedupStore, dl DeadLetterStore) *Dispatcher {
	logger, _ := testutil.CaptureLogger()
	return NewDispatcher(n, admins, dedup, dl, Config{RetryMax: 3, RetryBase: time.Millisecond}, logger)
}

// runConsumer запускает Consumer в фоне на свежей consumer-group и глушит его в
// t.Cleanup (отмена ctx + Close). Даём небольшую паузу после старта, чтобы
// консьюмер успел присоединиться к группе и начать чтение.
func runConsumer(t *testing.T, brokers []string, topic string, disp *Dispatcher) {
	t.Helper()
	logger, _ := testutil.CaptureLogger()
	c := NewConsumer(brokers, topic, "notifier-test-"+uuid.NewString(), disp, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	// Пауза для consumer group coordination: консьюмер должен присоединиться к группе
	// и получить назначение партиции от координатора.
	time.Sleep(200 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-done
		_ = c.Close()
	})
}

func evt(eventID, typ, bookingID, userID string) events.Event {
	return events.Event{
		EventID:   eventID,
		Type:      typ,
		BookingID: bookingID,
		UserID:    userID,
		RoomID:    "room-1",
		Timestamp: time.Now().UTC(),
	}
}

// publish пишет события в топик по одному, ключ — BookingID (как продюсер), что
// на односекционном топике сохраняет порядок публикации. Ретраит при UNKNOWN_TOPIC_OR_PARTITION,
// так как metadata топика может ещё не успеть распространиться.
func publish(t *testing.T, brokers []string, topic string, evs ...events.Event) {
	t.Helper()
	// Отключаем connection pool, чтобы избежать stale connections между тестами.
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		BatchTimeout:           10 * time.Millisecond,
		AllowAutoTopicCreation: false, // мы создаём топик явно в newTopic
	}
	defer func() { _ = w.Close() }()

	for _, ev := range evs {
		payload, err := json.Marshal(ev)
		require.NoError(t, err)
		// Ретрай на случай, если metadata топика ещё не propagated к писателю.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			err := w.WriteMessages(ctx, kafka.Message{
				Key:   []byte(ev.BookingID),
				Value: payload,
			})
			if err == nil {
				break
			}
			// UNKNOWN_TOPIC_OR_PARTITION (Error 3) → retry, metadata ещё не готов.
			if kafkaErrorCode(err) == 3 {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			require.NoError(t, err)
		}
	}
}

// newTopic создаёт уникальный топик с одной партицией — детерминированный порядок
// обработки для «барьерных» проверок. Ждёт, пока топик станет доступен для записи.
func newTopic(t *testing.T, brokers []string) string {
	t.Helper()
	topic := "booking.events." + uuid.NewString()

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

	// CreateTopics асинхронен: ждём, пока метаданные топика станут доступны и
	// продюсер сможет писать. Проверяем попыткой записи тестового сообщения.
	w := kafka.NewWriter(kafka.WriterConfig{
		Brokers:   brokers,
		Topic:     topic,
		BatchSize: 1,
	})
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for topic %s to become writable", topic)
		default:
		}
		err := w.WriteMessages(ctx, kafka.Message{
			Key:   []byte("probe"),
			Value: []byte("{}"),
		})
		if err == nil {
			// Топик готов — тестовое сообщение записалось
			return topic
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// kafkaErrorCode извлекает код ошибки из kafka.Error. Если ошибка не от Kafka — возвращает 0.
func kafkaErrorCode(err error) int {
	if err == nil {
		return 0
	}
	// kafka.Error это int, который удовлетворяет интерфейсу error
	if kerr, ok := err.(kafka.Error); ok {
		return int(kerr)
	}
	// Также проверяем обёрнутые ошибки
	var kerr kafka.Error
	if errors.As(err, &kerr) {
		return int(kerr)
	}
	return 0
}
