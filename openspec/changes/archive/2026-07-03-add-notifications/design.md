## Context

Сервис уже публикует доменные события бронирования в Kafka после коммита транзакции
через общий хелпер `publishBookingEvent` (`internal/service/booking.go`): сбой публикации —
деградация, не откат. Тело события (`internal/events/events.go`, `events.Event`) намеренно
несёт только идентификаторы (`type`, `booking_id`, `user_id`, `room_id`, `timestamp`) в
формате всего проекта (`b-<uuid>`, `user-<uuid>`, `room-<uuid>`); trace-контекст уходит в
заголовок сообщения (`traceparent`), а не в тело. Ключ сообщения — `booking_id`, поэтому
события одной брони упорядочены в одном партишне.

Сейчас потребителя событий нет. Change `add-large-room-approval` добавляет типы
`booking.pending_approval` / `booking.approved` / `booking.rejected` (и колонку
`rejection_reason`), но реализует только публикацию — потребление вынесено сюда, в парный
change. Нужен отдельный процесс, который читает `booking-events`, превращает события в
уведомления адресатам и доставляет их через абстракцию `Notifier` идемпотентно, с
at-least-once и dead-letter.

Слоистость проекта — `handler → service → repository`, поток только вниз, интерфейсы
объявляет потребитель. Транспорт (HTTP) не содержит бизнес-логики, авторизация/правила —
в service, SQL — в repository. Нотификатор повторяет эту раскладку: consumer (транспорт
Kafka) → dispatcher (бизнес-маппинг/дедуп/ретраи) → repository (SQL). Логи — только `slog`,
контекст (`trace_id`) тянется через `context.Context`.

## Goals / Non-Goals

**Goals:**
- Отдельный процесс `cmd/notifier`, читающий `booking-events` как consumer-group, с graceful
  shutdown; независим от HTTP-сервера.
- Пакет `internal/notifications/`: consumer, dispatcher, `Notifier` + `LogNotifier`.
- Маппинг 5 типов событий на адресатов (владелец / создатель / все админы) и содержимое
  уведомления; обогащение из БД (админы, причина отказа, детали брони).
- Идемпотентность через дедуп по `event_id` (устойчивое хранилище `processed_events`).
- At-least-once: ручной коммит offset только после завершения обработки.
- Retry с экспоненциальным backoff до `N` попыток, затем dead-letter (`notification_dead_letter`).
- Расширение контракта события полем `event_id` и проставление его продюсером.

**Non-Goals:**
- Реальная доставка (email/push/SMS): по умолчанию только `LogNotifier`.
- WebSocket real-time доставка — задокументирована как расширение поверх `Notifier`, но
  не реализуется в этом change (стек `net/http` без внешних WS-зависимостей — отдельный change).
- Exactly-once доставка: инвариант — at-least-once, дубли допустимы.
- UI/эндпоинт просмотра уведомлений и dead-letter (только таблица; операционный разбор — вручную).
- Настраиваемые шаблоны/локализация текстов уведомлений (тексты — константы).
- Изменение поведения HTTP-сервера и существующих правил бронирования.

## Decisions

### Решение 1: Отдельный процесс `cmd/notifier` на `kafka.Reader` с GroupID и ручным коммитом
Consumer запускается как самостоятельный бинарник (`cmd/notifier/main.go`), а не как
горутина в HTTP-сервере: независимое масштабирование и деплой, падение одного не роняет
другой. Читаем через `kafka.Reader` с `GroupID` (consumer-group: партиции делятся между
инстансами, offset хранится в Kafka). Коммит offset — **ручной** (`CommitMessages`) и только
**после** завершения обработки сообщения — это и есть механизм at-least-once.
- **Почему не встраивать в сервер**: смешало бы жизненные циклы и деплой HTTP и consumer.
- **Почему GroupID, а не ручное управление партициями**: балансировка и хранение offset «из
  коробки», горизонтальное масштабирование нотификатора добавлением инстансов в группу.
- **Почему ручной коммит**: авто-коммит по таймеру может закоммитить offset до обработки →
  потеря события. Ручной коммит после обработки даёт at-least-once.

### Решение 2: `event_id` в теле события + дедуп в таблице `processed_events`
Дедупликация «по event ID» требует стабильного идентификатора события. В `events.Event`
добавляется `EventID string` (`event_id`, формат `evt-<uuid>`); `publishBookingEvent`
генерирует его при публикации. Consumer перед отправкой проверяет `processed_events` по
`event_id`; после успешной обработки вставляет `event_id` туда. Порядок строгий:
**сначала обработать (отправить/в dead-letter), потом зафиксировать `event_id`, потом
закоммитить offset.**
- **Почему `event_id` в теле, а не ключ Kafka**: ключ сообщения — `booking_id` (для
  упорядочивания в партишне), он не уникален на событие. Offset не годится как ключ дедупа
  (меняется при перечитывании/ребалансе). Нужен идентификатор, стабильный именно для события.
- **Почему таблица, а не in-memory set**: in-memory теряется при рестарте → идемпотентность
  ломается между перезапусками. Postgres уже в стеке, таблица переживает рестарт.
- **Почему обработка перед фиксацией**: если фиксировать `event_id` до отправки, сбой отправки
  оставит событие «обработанным» и потеряет уведомление. Обработка-затем-фиксация сохраняет
  at-least-once; дедуп ловит повторную доставку уже завершённого события (полный повтор), а
  падение между отправкой и фиксацией даёт дубль — допустимо по инварианту.

### Решение 3: Раскладка слоёв `consumer → dispatcher → repository`
- **consumer** (транспорт Kafka): цикл `FetchMessage`, восстановление trace-контекста через
  `events.ContextFromMessage`, декод `events.Event`, вызов dispatcher, `CommitMessages`.
  Бизнес-логики нет — как HTTP-handler.
- **dispatcher** (бизнес-слой, аналог service): дедуп-проверка, маппинг тип→адресаты,
  обогащение из БД, вызов `Notifier.Send` с retry/backoff, dead-letter, фиксация `event_id`.
  Тексты/типы уведомлений — здесь. Интерфейсы зависимостей (`Notifier`, `AdminLookup`,
  `BookingLookup`, `DedupStore`, `DeadLetterStore`) объявляются **в dispatcher** (потребителе).
- **repository** (SQL): `ListAdmins`, чтение брони, `MarkProcessed`/`IsProcessed`,
  `SaveDeadLetter`. Возвращает `model.*`, нормализует `pgx.ErrNoRows`.

### Решение 4: `Notifier` и адресация по строковому ID проекта (отклонение от `uuid.UUID`)
Интерфейс: `Notifier.Send(ctx context.Context, userID string, n Notification) error`.
`Notification` — структура с полями `Type` (напр. `booking_confirmed`), `BookingID`,
`RoomID`, `Title`, `Message` и опц. `Reason`. `LogNotifier` пишет её через `slog` (без PII).
- **Отклонение от запроса**: в задаче интерфейс указан как `Send(ctx, userID uuid.UUID, ...)`,
  но весь проект использует **строковые ID с префиксом** (`user-<uuid>`); `repository.User.Get`
  принимает `id string`, события несут `user_id` строкой. Приведение к `uuid.UUID` заставило бы
  срезать/возвращать префикс на каждой границе и разошлось бы с конвенцией именования. Поэтому
  адресат — `string` (`user-<uuid>`). Это осознанное отклонение (см. Open Questions).
- **Почему заглушка `LogNotifier`**: доставка вне объёма; интерфейс изолирует dispatcher от
  канала. Реальные каналы (email/WS) добавляются позже без изменения dispatcher.

### Решение 5: Retry с backoff в процессе + dead-letter как таблица Postgres
При ошибке `Send` — до `N` попыток с экспоненциальной задержкой (`base * 2^i`, конфиг
`NOTIFY_RETRY_MAX`, `NOTIFY_RETRY_BASE`), прерывается по `ctx.Done()`. После исчерпания —
запись в таблицу `notification_dead_letter` (`event_id`, `user_id`, `type`, `error`,
`created_at`) и обработка события считается завершённой (offset коммитится), чтобы
«отравленное» сообщение не блокировало партицию.
- **Почему dead-letter в БД, а не отдельный Kafka-топик**: не требует producer'а в
  нотификаторе и настройки нового топика; запись сразу queryable для операционного разбора.
  Постгрес уже в стеке. (Альтернатива — DLQ-топик `booking-events.dlq` — оставлена на будущее,
  интерфейс `DeadLetterStore` позволяет заменить реализацию.)
- **Почему не блокировать на «отравленном» сообщении**: бесконечный ретрай остановил бы всю
  партицию. Dead-letter + коммит сохраняют прогресс, недоставленное не теряется (лежит в DLQ).

### Решение 6: Миграция `005_notifications` (только новые таблицы, без изменения существующих)
`005_notifications.up.sql`: `CREATE TABLE processed_events (event_id TEXT PRIMARY KEY,
event_type TEXT NOT NULL, processed_at TIMESTAMPTZ NOT NULL DEFAULT now())` и
`CREATE TABLE notification_dead_letter (id BIGSERIAL PRIMARY KEY, event_id TEXT NOT NULL,
user_id TEXT NOT NULL, notification_type TEXT NOT NULL, error TEXT NOT NULL,
created_at TIMESTAMPTZ NOT NULL DEFAULT now())`. `.down.sql` дропает обе. Существующие
таблицы не трогаем — нотификатор читает `users`/`bookings` read-only.

## Risks / Trade-offs

- **Дубли уведомлений при падении между отправкой и фиксацией `event_id`** → допустимо по
  инварианту at-least-once; дедуп ловит только полный повтор уже завершённого события.
- **Рост `processed_events`** → таблица растёт монотонно. Митигигация: периодическая очистка
  записей старше retention (напр. TTL-джоб/`DELETE ... WHERE processed_at < now()-30d`) —
  вне объёма, отметить как операционную задачу.
- **`event_id` отсутствует у «старых» событий** (опубликованных до деплоя продюсера с полем)
  → дедуп по пустому ключу ненадёжен. Митигигация: если `event_id` пуст, dispatcher
  логирует `Warn` и обрабатывает без дедупа (fallback), либо генерирует ключ из
  `type|booking_id|timestamp`. Выбрать fallback-ключ в реализации.
- **Список админов меняется во времени** → уведомляются админы на момент обработки события;
  это соответствует требованию и приемлемо.
- **Рассинхрон топика продюсера и consumer'а** → продюсер по умолчанию пишет в `booking.events`
  (env `KAFKA_TOPIC`), а в задаче упомянут `booking-events`. Consumer MUST читать тот же
  топик: используем общий env `KAFKA_TOPIC` с тем же дефолтом `booking.events`. Согласовать
  значение в `docker-compose.yml` для обоих сервисов.
- **Обогащение из БД связывает нотификатор со схемой `bookings`/`users`** → read-only доступ,
  без записи в чужие таблицы; изменение схемы этих таблиц может затронуть нотификатор.
- **Порядок против частичной обработки для нескольких админов** → если `Send` части админов
  упал, событие пойдёт в dead-letter для недоставленных, но уже доставленным при повторе
  прилетит дубль. Допустимо (at-least-once).

## Migration Plan

1. Применить миграцию `005_notifications` (создаёт `processed_events`,
   `notification_dead_letter`); безопасна — только новые таблицы.
2. Задеплоить продюсер с `event_id` в `publishBookingEvent` (обратносовместимо: новое поле
   в JSON, старые consumer'ы его игнорируют).
3. Задеплоить `cmd/notifier` как отдельный сервис (`docker-compose.yml`: сервис `notifier`
   с `DATABASE_URL`, `KAFKA_BROKERS`, `KAFKA_TOPIC`, `KAFKA_GROUP_ID`, retry-конфиг).
4. Rollback: остановить сервис `notifier` (события накапливаются в топике по retention,
   не теряются); при откате продюсера поле `event_id` просто исчезает из новых событий.
   Миграцию откатывать не обязательно (таблицы изолированы); при необходимости — `.down.sql`.

## Open Questions

- **Сигнатура `Notifier.Send`**: принято `userID string` (`user-<uuid>`) вместо запрошенного
  `uuid.UUID` ради конвенции проекта. Подтвердить или вернуть `uuid.UUID` (тогда парсинг
  префикса на границах).
- **Fallback дедупа при пустом `event_id`** (старые события): обрабатывать без дедупа с `Warn`
  или синтезировать ключ `type|booking_id|timestamp`? По умолчанию — синтетический ключ.
- **Объём обогащения `booking.rejected`**: причина берётся из `bookings.rejection_reason`
  (нужен `Get` брони). Нужны ли в тексте название комнаты/интервал (доп. чтение `rooms`)?
  По умолчанию — `booking_id`, `room_id`, интервал из брони; название комнаты — опционально.
- **Retention/очистка `processed_events`** — кто и как чистит (вне объёма change).
