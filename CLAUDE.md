# CLAUDE.md

Система бронирования переговорных комнат в коворкинге: REST API на Go, PostgreSQL,
JWT-аутентификация. Комнаты, брони с проверкой пересечений, ролевой доступ (admin/manager/member).

## Стек

- **Go 1.25**, роутер `go-chi/chi/v5`, логи `log/slog` (JSON).
- **PostgreSQL** через `jackc/pgx/v5` (`pgxpool`), сырой SQL (без ORM).
- **JWT** — `golang-jwt/jwt/v5` (HS256, `sub` = user id).
- **Тесты** — `stretchr/testify`, интеграционные на `testcontainers-go` + модуль `postgres`.

## Структура

- `cmd/server/` — точка входа HTTP-сервера: конфиг из env, wiring слоёв, graceful shutdown.
- `cmd/notifier/` — отдельный процесс-потребитель событий бронирования из Kafka, рассылает
  уведомления пользователям через `Notifier` (по умолчанию `LogNotifier`). Запускается
  независимо от HTTP-сервера, подключаясь к тем же БД и Kafka. См. `internal/notifications`.
- `internal/handler/` — HTTP: роутинг, JSON, auth-middleware, маппинг ошибок.
- `internal/service/` — бизнес-логика, валидация, авторизация.
- `internal/repository/` — доступ к БД (SQL).
- `internal/model/` — доменные типы и enum'ы (общие для всех слоёв).
- `internal/testutil/` — общий setup БД для интеграционных тестов.
- `migrations/` — SQL-миграции `NNN_name.up.sql` / `.down.sql` (применяются по возрастанию).
- `api/openapi.yaml` — спецификация API; `docs/test-scenarios.md` — сценарии.

## Слоистость: handler → service → repository

Поток идёт только вниз; слой ниже не знает о слое выше.

- **handler** — разбор запроса, `writeJSON`, `writeServiceError` (доменная ошибка → HTTP-код).
  Достаёт `Actor` из контекста (`actorFromCtx`). **Никакой бизнес-логики.**
- **service** — все правила: валидация полей, длительность/дедлайны брони, проверка ролей.
  `Actor` передаётся явным аргументом (авторизация здесь, не в handler). Время — через
  поле `now func() time.Time` (подменяется в тестах). Работает с моделями и интерфейсами репо.
- **repository** — только SQL, возвращает `model.*`. Нормализует `pgx.ErrNoRows` → `ErrNotFound`
  (`wrapNoRows`). Атомарные операции в транзакции (напр. `CreateChecked`, Serializable).

Интерфейсы объявляются в **потребителе** (service определяет `BookingRepo`, `RoomLookup`;
handler — `UserLookup`), а не в реализующем пакете — это и есть граница слоёв.

## Конвенции именования

- Один файл на сущность в слое: `booking.go`, `room.go`, `user.go`.
- Структуры слоя — существительное-сущность: `service.Booking`, `repository.Booking`;
  конструктор `NewBooking(...)`. Различай по пакету, суффиксы `Service`/`Repo` не в имени типа.
- Входные DTO сервиса — `XxxCreateInput`; тела HTTP-запросов — `xxxCreateBody` (unexported).
- Хендлеры — unexported `verbNoun`: `createBooking`, `listRooms`, `searchAvailableRooms`.
- Ошибки: sentinel `ErrXxx` (snake_case-значение, напр. `ErrRoomNotFound`) в `service/errors.go`;
  типизированные — `ValidationError`, `BookingConflictError`. HTTP-коды ошибок — `SCREAMING_SNAKE`.
- ID сущностей — префикс + uuid: `b-<uuid>`, `room-<uuid>`, `user-<uuid>`.
- Комментарии и доменные термины — на русском (следуй существующему стилю).

## Тесты

- **Unit (service)** — пакет `service`, без БД. Моки — ручные структуры с полями-функциями
  (`getFn`, `createCheckedFn`, ...); незаданный метод паникует. Фикстуры — константы `testXxxID`.
  Запуск: `go test ./internal/service/...`.
- **Интеграционные (repository)** — пакет `repository_test`, реальный Postgres.
  `testutil.SetupTestDB(t)` через `sync.Once` поднимает **один** контейнер на тестовый бинарник,
  применяет `migrations/`, возвращает пул и `cleanup` (TRUNCATE всех таблиц — вызывать в каждом
  подтесте для изоляции). Данные сеются хелперами `seedRoom`/`seedUser`. **Нужен Docker.**
  Сборочных тегов нет; фильтруй пакетом: `go test ./internal/repository/...`.

## Хелперы тестов (internal/testutil)

Не собирай фикстуры вручную — используй хелперы. Дефолты детерминированы
(константы `RoomID`/`UserID`/`OtherUserID`/`AdminID`/`BookingID`, якоря времени
`FixedNow`, `BaseStart`, `BaseEnd`, зона `MSK`), поэтому сравнения на равенство
предсказуемы.

- **Фикстуры (функциональные опции)** — возвращают `model.*`, применяют опции по порядку:
  - `Room(opts...)` — опции `WithRoomID`, `WithFloor`, `WithCapacity`, `WithRoomStatus`, `WithEquipment`.
  - `User(opts...)` — опции `WithUserID`, `WithRole`, `WithManagesFloor`, `WithEmail`.
  - `Booking(opts...)` — опции `WithBookingID`, `WithRoom`, `WithOwner`, `WithStart`,
    `WithInterval`, `WithDuration`, `WithBookingStatus`, `WithTitle`.
- **Билдер брони (цепочечный)** — `NewBookingBuilder(t)` с
  `WithRoom(model.Room)` / `WithUser(model.User)` / `WithTime(start, end)` /
  `WithStatus(string)` и терминальным `Build() model.Booking`. Сам подставляет
  дефолтные комнату и пользователя (member), если не заданы, и выдаёт брони свежий uuid.
- **Часы** — `Clock(now)` возвращает `func() time.Time` для подмены поля `now` сервиса.
- **Ассерты ошибок** — `AssertServiceError(t, err, wantIs, wantAs)`,
  `AssertSentinel(t, err, want)`, `AssertTyped[E](t, err) E`.
- **Интеграционные (нужен Docker)** — `SetupTestDB(t)` (пул + cleanup),
  сиды `SeedRoom`/`SeedUser`/`SeedBooking` (принимают те же опции фикстур),
  `FreshRoomAndUser(t, pool, cleanup)` — типовой пролог подтеста.

Builder — когда нужен конкретный набор полей. Object mother — когда нужен «просто объект». Не создавать model.Booking{...} руками в тестах.

## Правила

- **Новый код обязан иметь тесты.** Бизнес-правила → unit-тест в service; новый SQL → интеграционный
  тест в repository.
- **Бизнес-логика — только в service.** Handler не валидирует и не авторизует, repository не
  принимает решений (только читает/пишет).
- Новая доменная ошибка: добавь sentinel в `service/errors.go` **и** ветку в `handler/errors.go`.
- Изменение схемы БД — только через новую пару миграций в `migrations/`.
- Запуск всего стека: `docker compose up` (env: `DATABASE_URL`, `JWT_SECRET`, `HTTP_ADDR`).

## Режимы работы

### architect
- Не пиши код. Только анализ и план.
- Предложи структуру: какие файлы создать/изменить, какие интерфейсы затронуты.
- Укажи риски: concurrency, миграции, обратная совместимость.
- Формат: markdown с пунктами, без кода.

### review
- Проверь код по чеклисту: правильное архитектурное разделение, обработка ошибок, SQL-инъекции, конкурентный доступ.
- Для каждой проблемы: файл, строка, что не так, как исправить.
- Не исправляй сам — только перечисли.

### implement
- Режим по умолчанию. Пиши код, следуя конвенциям из этого CLAUDE.md.
- Новый код — с тестами. Без тестов не коммитим.

## Контекст

При работе с бизнес-логикой (service, repository) не загружай:
- docker-compose.yml
- migrations/ (используй схему из комментария в модели)
- web/ (фронтенд, не относится к API)

## Observability

### Логирование
- Только структурный логгер `log/slog`. Никаких `fmt.Println`, `log.Printf`.
- Уровни: `Info` — успешные изменения состояния (бронь создана/отменена, событие опубликовано); `Warn` — деградация, на которой сервис выжил (Kafka недоступна, фолбэк на БД мимо кэша); `Error` — операция провалилась, всегда с `slog.Any("error", err)`.
- Контекстные поля одинаковыми ключами во всех логах операции: `request_id`, `trace_id`, `booking_id`, `user_id`, `room_id`. Прокидывать через `slog.With(...)` от входа в сервис, а не дублировать в каждом вызове.
- Никогда не логировать секреты и PII: токены, пароли, заголовки авторизации, полные тела запросов. При сомнении — не логировать.
- Логгер инжектится через конструктор, а не глобальный.

### Трейсинг и correlation ID
- На входе HTTP-хендлера: взять `traceparent` из заголовка, если нет — сгенерировать. Положить в `context.Context`.
- Весь путь запроса (handler → service → repository → cache) тянет один и тот же `context.Context`. ID не передаётся отдельным аргументом.
- При публикации в Kafka trace-контекст пробрасывается в **заголовки сообщения** (`kafka.Header` с `traceparent`), а не в тело `Event`. Consumer восстанавливает контекст из заголовков — так трейс не рвётся на границе сервисов.
- `trace_id` из контекста попадает в каждый лог — по нему сшиваются логи одного запроса через все компоненты.