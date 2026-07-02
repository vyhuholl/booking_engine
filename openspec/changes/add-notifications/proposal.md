## Why

Смена статуса брони уже публикует доменные события в Kafka
(`booking.created` / `booking.cancelled`, а с change `add-large-room-approval` —
`booking.pending_approval` / `booking.approved` / `booking.rejected`), но их никто
не потребляет: владелец брони не узнаёт о подтверждении/отмене, админы не видят, что
появилась бронь на согласование, создатель не получает вердикт по approve/reject.
Нужен потребитель этих событий, который превращает их в уведомления пользователям.

## What Changes

- Вводится **отдельный процесс-нотификатор** (`cmd/notifier/`), запускаемый независимо
  от HTTP-сервера. Он читает топик `booking-events` из Kafka как consumer-group и
  рассылает уведомления. HTTP-сервер не меняет поведение.
- Новый пакет `internal/notifications/`: consumer, маппинг событий в уведомления,
  интерфейс доставки `Notifier` и заглушка `LogNotifier` (пишет уведомление в лог).
- Маппинг событий на адресатов:
  - `booking.created` → владелец брони (подтверждение).
  - `booking.cancelled` → владелец брони (отмена).
  - `booking.pending_approval` → **все** пользователи с ролью `admin` (нужно одобрение).
  - `booking.approved` → создатель брони (одобрено).
  - `booking.rejected` → создатель брони (отклонено, с причиной).
- **Идемпотентность / дедупликация по event ID**: повторная обработка одного и того же
  события не порождает повторных уведомлений. Требует стабильного идентификатора
  события в шине.
- **Гарантия at-least-once**: уведомление может быть доставлено более одного раза
  (дубли допустимы); потеря события недопустима.
- **Retry с backoff и dead-letter**: при ошибке доставки уведомления — повторные попытки
  с экспоненциальной задержкой; после исчерпания N попыток событие уходит в
  dead-letter, обработка не блокирует остальные сообщения.
- **BREAKING (контракт события)**: в тело `events.Event` добавляется поле `event_id`
  (стабильный идентификатор события, формат `evt-<uuid>`). Продюсер (`publishBookingEvent`)
  обязан его проставлять; без него дедупликация невозможна. Существующие поля не меняются.
- Опционально (не в объёме этого change по умолчанию): WebSocket-доставка для
  real-time уведомлений — задокументирована как расширение поверх интерфейса `Notifier`.

## Capabilities

### New Capabilities
- `notifications`: потребление доменных событий бронирования из Kafka отдельным
  процессом и доставка уведомлений адресатам через интерфейс `Notifier`; маппинг
  типов событий на получателей, идемпотентная обработка (дедуп по event ID),
  at-least-once, retry с backoff и dead-letter.

### Modified Capabilities
<!-- Контракт events.Event расширяется полем event_id, но отдельного spec для
     возможности «события» в openspec/specs/ нет — изменение зафиксировано в Impact
     и подробно разобрано в design.md. Требований существующих capability
     (rooms-list, booking-waitlist, large-room-approval) этот change не меняет. -->

## Impact

- **Новый процесс**: `cmd/notifier/main.go` — конфиг из env (`DATABASE_URL`,
  `KAFKA_BROKERS`, `KAFKA_TOPIC=booking-events`, `KAFKA_GROUP_ID`, backoff/retry-настройки),
  wiring слоёв, graceful shutdown. `docker-compose.yml` получает сервис `notifier`.
- **Новый пакет**: `internal/notifications/` — consumer, диспетчер событий, `Notifier`
  + `LogNotifier`, retry/backoff, dead-letter.
- **Контракт событий** (`internal/events/events.go`): поле `EventID string` (`event_id`)
  в `Event`; `publishBookingEvent` (`internal/service/booking.go`) проставляет его при
  публикации. Затрагивает всех продюсеров и обоих consumer'ов.
- **БД / миграции** (`migrations/`): новая пара `005_*` — таблица дедупликации
  обработанных событий (`processed_events`) и таблица `notification_dead_letter`.
  Нотификатору нужен read-only доступ к `users` (список админов) и `bookings`
  (обогащение уведомления: комната, время, причина отказа).
- **Зависимости**: consumer поверх уже используемого `segmentio/kafka-go` (Reader/
  consumer-group). Новых внешних зависимостей не требуется.
- **Порядок с `add-large-room-approval`**: типы событий approve/reject вводятся тем
  change; нотификатор обрабатывает их, поэтому применяется после него (или совместно).
