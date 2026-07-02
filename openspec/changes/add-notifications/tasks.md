## 1. Контракт события: event_id

- [ ] 1.1 Добавить в `internal/events/events.go` поле `EventID string` (`json:"event_id"`) в `Event`
- [ ] 1.2 В `publishBookingEvent` (`internal/service/booking.go`) генерировать `EventID` в формате `evt-<uuid>` при сборке события
- [ ] 1.3 Обновить/добавить unit-тест на `publishBookingEvent`/сериализацию `Event`: в опубликованном событии `event_id` непустой и в формате `evt-<uuid>`; существующие поля не изменились

## 2. Миграция и модель

- [ ] 2.1 Создать `migrations/005_notifications.up.sql`: таблица `processed_events (event_id TEXT PRIMARY KEY, event_type TEXT NOT NULL, processed_at TIMESTAMPTZ NOT NULL DEFAULT now())`
- [ ] 2.2 В той же миграции: таблица `notification_dead_letter (id BIGSERIAL PRIMARY KEY, event_id TEXT NOT NULL, user_id TEXT NOT NULL, notification_type TEXT NOT NULL, error TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`
- [ ] 2.3 Создать `migrations/005_notifications.down.sql`: `DROP TABLE notification_dead_letter`, `DROP TABLE processed_events`
- [ ] 2.4 Добавить доменные типы уведомления в `internal/model/model.go`: константы типов (`NotificationBookingConfirmed`, `...Cancelled`, `ApprovalRequested`, `...Approved`, `...Rejected`)

## 3. Repository (SQL для нотификатора)

- [ ] 3.1 Добавить `ListAdmins(ctx) ([]model.User, error)` в `internal/repository/user.go` (`SELECT ... WHERE role = 'admin'`)
- [ ] 3.2 Убедиться, что `repository.Booking.Get` возвращает `rejection_reason` (зависит от `add-large-room-approval`; если поля нет — обогащение причиной опустить)
- [ ] 3.3 Создать `internal/repository/notification.go`: `Notification` c методами `IsProcessed(ctx, eventID) (bool, error)`, `MarkProcessed(ctx, eventID, eventType) error` (`INSERT ... ON CONFLICT (event_id) DO NOTHING`), `SaveDeadLetter(ctx, dl model.DeadLetter) error`; `wrapNoRows` где нужно
- [ ] 3.4 Интеграционный тест (`repository_test`, Docker): `ListAdmins` возвращает только админов; `MarkProcessed` + `IsProcessed` идемпотентны (повторный `MarkProcessed` того же `event_id` не падает); `SaveDeadLetter` пишет запись

## 4. Notifier: интерфейс и заглушка

- [ ] 4.1 Создать `internal/notifications/notifier.go`: тип `Notification` (`Type`, `BookingID`, `RoomID`, `Title`, `Message`, `Reason`) и интерфейс `Notifier.Send(ctx context.Context, userID string, n Notification) error`
- [ ] 4.2 Реализовать `LogNotifier` (`internal/notifications/log_notifier.go`): пишет через инжектируемый `*slog.Logger` поля `user_id`, `type`, `booking_id` (без PII/секретов), возвращает `nil`
- [ ] 4.3 Unit-тест `LogNotifier`: `Send` логирует ожидаемые поля и не возвращает ошибку (подмена `slog.Handler` буфером)

## 5. Dispatcher (бизнес-логика обработки события)

- [ ] 5.1 Создать `internal/notifications/dispatcher.go`: объявить интерфейсы-зависимости `Notifier`, `AdminLookup` (`ListAdmins`), `BookingLookup` (`Get`), `DedupStore` (`IsProcessed`/`MarkProcessed`), `DeadLetterStore` (`SaveDeadLetter`); конструктор `NewDispatcher(...)` с полем `now func() time.Time` и retry-конфигом
- [ ] 5.2 Реализовать маппинг тип→адресаты+`Notification`: `booking.created`→владелец; `booking.cancelled`→владелец; `booking.pending_approval`→все из `ListAdmins`; `booking.approved`→создатель; `booking.rejected`→создатель+`Reason` из брони; неизвестный тип → лог `Info`, no-op
- [ ] 5.3 Реализовать `Handle(ctx, ev events.Event) error`: дедуп-проверка по `event_id` (fallback-ключ при пустом `event_id` — синтетический `type|booking_id|timestamp` + `Warn`); при уже обработанном — no-op; иначе отправка адресатам, затем `MarkProcessed`
- [ ] 5.4 Реализовать retry с экспоненциальным backoff (`base * 2^i`, до `N` попыток, прерывание по `ctx.Done()`); после исчерпания — `SaveDeadLetter` для недоставленного и продолжить (обработку считать завершённой)
- [ ] 5.5 Прокидывать `slog.With(trace_id, event_id, booking_id, user_id)` от входа `Handle`; логировать успехи `Info`, деградацию `Warn`, провалы `Error` с `slog.Any("error", err)`
- [ ] 5.6 Unit-тесты dispatcher (моки-структуры с полями-функциями, стиль `testutil`): маппинг каждого из 5 типов на верных адресатов и содержимое; `pending_approval` рассылает всем админам; дедуп — повторный `Handle` того же `event_id` не шлёт повторно; retry: успех со 2-й попытки; dead-letter после N провалов + offset-семантика (возврат без ошибки); неизвестный тип — no-op; пустой `event_id` — fallback-ключ

## 6. Consumer (транспорт Kafka)

- [ ] 6.1 Создать `internal/notifications/consumer.go`: `Consumer` поверх `kafka.Reader` (`GroupID`, `Topic`, `Brokers`), конструктор принимает `Dispatcher` и `*slog.Logger`
- [ ] 6.2 Реализовать `Run(ctx)`: цикл `FetchMessage` → `events.ContextFromMessage` (восстановить trace-контекст) → `json.Unmarshal` в `events.Event` → `dispatcher.Handle` → `CommitMessages` (ручной коммит только после обработки = at-least-once); выход по `ctx.Done()`
- [ ] 6.3 Обработка ошибки декода сообщения: лог `Error`, коммит offset (иначе «отравленное» сообщение блокирует партицию) — не ретраить нераспарсиваемое
- [ ] 6.4 Реализовать `Close()` — закрытие `kafka.Reader`
- [ ] 6.5 Тест consumer: unmarshal `events.Event` из сообщения + восстановление trace-контекста из заголовка (переиспользовать `events.ContextFromMessage`); интеграционный тест чтения из реальной Kafka — опционально, если есть `testutil.Kafka` (Docker)

## 7. Процесс cmd/notifier

- [ ] 7.1 Создать `cmd/notifier/main.go`: конфиг из env (`DATABASE_URL`, `KAFKA_BROKERS`, `KAFKA_TOPIC` дефолт `booking.events`, `KAFKA_GROUP_ID` дефолт `notifier`, `NOTIFY_RETRY_MAX`, `NOTIFY_RETRY_BASE`); валидация обязательных
- [ ] 7.2 Wiring: `pgxpool` (+ping с ретраями), `repository.NewUser`/`NewBooking`/`NewNotification`, `LogNotifier`, `NewDispatcher`, `NewConsumer`; `slog` JSON-логгер
- [ ] 7.3 Graceful shutdown: `signal.NotifyContext(SIGINT, SIGTERM)`; `consumer.Run(rootCtx)`; по завершении — `consumer.Close()` и `pool.Close()`
- [ ] 7.4 Добавить сервис `notifier` в `docker-compose.yml` (тот же образ/сборка, команда `./notifier`, env согласован с сервером: **тот же `KAFKA_TOPIC`**)

## 8. Документация

- [ ] 8.1 Обновить `CLAUDE.md`/`README`: описать процесс `cmd/notifier`, env-переменные, инвариант at-least-once + дедуп
- [ ] 8.2 Отметить в `docs/test-scenarios.md` (при наличии) сценарии уведомлений: подтверждение/отмена/запрос одобрения/одобрение/отклонение, дедуп, dead-letter

## 9. Проверка

- [ ] 9.1 `go build ./...` и `go vet ./...` проходят
- [ ] 9.2 `go test ./internal/service/... ./internal/notifications/...` (unit) зелёные
- [ ] 9.3 `go test ./internal/repository/...` (интеграционные, Docker) зелёные
