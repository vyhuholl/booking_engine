## Context

Сейчас `POST /bookings` всегда создаёт бронь в статусе `confirmed` (enum
`booking_status` = `confirmed | cancelled`). Занятость слота определяется предикатом
`status = 'confirmed'`, продублированным в трёх местах репозитория: `CreateChecked`
(атомарная вставка с проверкой пересечений, Serializable), `IsRoomBusy` (используется
листом ожидания) и `ListConflicting`. Авторизация и все бизнес-правила — в `service`;
handler только разбирает запрос и мапит ошибки; repository — только SQL. Доменные
события публикуются в Kafka после коммита через общий хелпер `publishBookingEvent`
(сбой публикации = деградация, не откат).

Требуется добавить workflow одобрения для комнат с `capacity > 12`: бронь создаётся на
согласовании, admin одобряет/отклоняет, слот резервируется на время рассмотрения, есть
24-часовой таймаут, идемпотентность и конкурентная безопасность. Consumer Kafka-событий
делается отдельным парным change — здесь только публикация.

## Goals / Non-Goals

**Goals:**
- Ветка `pending_approval` в жизненном цикле брони с ответом 202 для больших комнат.
- Резервирование слота на время `pending_approval`/`approved` (единый предикат занятости).
- Admin-эндпоинты списка/одобрения/отклонения с сохранением причины отказа.
- Ленивый 24-часовой таймаут без cron.
- Идемпотентность по статусу и безопасность конкурентных approve/reject через одну
  атомарную условную операцию.
- Приоритет отмены пользователем над одобрением.
- Отклонение освобождает слот и предлагает его листу ожидания (как отмена).
- Публикация `booking.pending_approval` / `booking.approved` / `booking.rejected`.

**Non-Goals:**
- Kafka consumer / реальная доставка уведомлений админу (отдельный change; «уведомление»
  здесь — событие в шину + список `GET /admin/approvals`).
- Отдельная подсистема нотификаций, email/push.
- Хранение идентификатора одобрившего/отклонившего админа (сохраняем только причину).
- Настраиваемый порог вместимости и настраиваемое окно таймаута (константы в service).
- Разрешение конфликта нескольких `pending_approval` на один слот: предикат занятости
  не даёт создать пересекающийся `pending_approval`, поэтому «очереди на одобрение» на
  один слот не возникает.

## Decisions

### Решение 1: Расширить `booking_status`, а не заводить отдельную таблицу approvals
Approval — это состояние жизненного цикла самой брони, а не отдельная сущность. Добавляем
в enum `booking_status` значения `pending_approval`, `approved`, `rejected` и колонки
`rejection_reason TEXT` (nullable) и `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`
(якорь таймаута) в таблицу `bookings`.
- **Почему не отдельная таблица**: пришлось бы джойнить при каждой проверке занятости и
  синхронизировать два источника истины о слоте. Одна таблица переиспользует существующий
  SQL проверки пересечений.
- **Модель**: `model.Booking` получает `RejectionReason *string` (omitempty) и
  `CreatedAt time.Time` — момент создания брони, якорь ленивого таймаута. Заполняется
  репозиторием из колонки `created_at` (аналог `WaitlistEntry.OfferedAt`). В JSON
  `created_at` можно не отдавать (внутренний якорь), но поле модели необходимо, чтобы
  таймаут проверялся в service (см. Решение 5).

### Решение 2: Единый предикат «активной» брони
Вводим набор занимающих слот статусов: `confirmed`, `pending_approval`, `approved`.
Все три места (`CreateChecked`, `IsRoomBusy`, `ListConflicting`) меняют условие
`status = 'confirmed'` на `status IN ('confirmed','pending_approval','approved')`.
- Централизуем список активных статусов (константа/хелпер в service — `model` или
  `service`) и в SQL используем один и тот же `IN`-набор, чтобы предикаты не разошлись.
- Это меняет и семантику `booking-waitlist` (см. delta-spec): встать в очередь можно на
  слот, удерживаемый `pending_approval`/`approved` бронью.

### Решение 3: Выбор статуса и кода ответа
- **Service** решает статус по размеру комнаты: `room.Capacity > 12` →
  `StatusPendingApproval`, иначе `StatusConfirmed`. Порог — константа
  `LargeRoomCapacityThreshold = 12` (строго больше).
- **Handler** выбирает HTTP-код по статусу возвращённой брони:
  `pending_approval` → 202, иначе 201. Никакой бизнес-логики в handler — только
  маппинг статуса на код (как маппинг ошибок на коды).

### Решение 4: Approve/Reject — атомарная условная операция в репозитории
Два метода репозитория, оба Serializable, оба с условием `WHERE id=$1 AND
status='pending_approval' RETURNING ...`:
- `Approve(ctx, id, now) (model.Booking, error)` — `UPDATE ... SET status='approved'`.
- `RejectAndOfferWaitlist(ctx, id, reason, now) (model.Booking, *model.WaitlistEntry, error)`
  — `UPDATE ... SET status='rejected', rejection_reason=$reason` и в **той же транзакции**
  предложение освободившегося слота листу ожидания (Решение 9). Зеркалит существующий
  `CancelAndOfferWaitlist`.
- 1 строка → успех. 0 строк → бронь не в `pending_approval` (уже approved/rejected/
  cancelled): репозиторий возвращает `ErrNotFound`-эквивалент. Это одновременно даёт
  **идемпотентность по статусу** и **безопасность гонки** (второй из двух одновременных
  approve получает 0 строк).
- Различение «не найдена» vs «не pending» — в сервисе: сначала `Get` для 404
  (`ErrApprovalNotFound`), затем проверки статуса/таймаута (Решение 5) и условный метод;
  `ErrNotFound` от репо → `ErrNotPendingApproval` (409).

### Решение 5: Ленивый таймаут через `Booking.CreatedAt` (проверка в service)
Окно `ApprovalTimeout = 24 * time.Hour` от момента создания брони. Проверяется **в
service** — как `OfferTTL` в `Waitlist.Confirm`: после `Get` сервис сравнивает
`now - b.CreatedAt > ApprovalTimeout`. Поэтому `created_at` вынесен в модель
(`Booking.CreatedAt`, Решение 1), а не остаётся только колонкой БД — иначе таймаут был бы
не проверить на сервисном слое (и не покрыть unit-тестом).
- **В `Approve`**: если бронь просрочена — сервис авто-отклоняет её через
  `RejectAndOfferWaitlist` с причиной-сентинелом (слот освобождается и предлагается очереди,
  Решение 9), публикует `booking.rejected` и возвращает `ErrNotPendingApproval` — одобрить
  просроченную нельзя.
- **В `ListPendingApprovals`**: репозиторий перед выборкой «подметает» просроченные —
  `UPDATE ... SET status='rejected', rejection_reason=<auto> WHERE status='pending_approval'
  AND created_at < now()-24h RETURNING id ...`; сервис публикует `booking.rejected` для
  подметённых и возвращает оставшиеся `pending_approval` (пакетное подметание в БД дешевле,
  чем читать каждую бронь на сервис для сравнения времени).
- Причина авто-отклонения — константа-сентинел (напр. `"approval timeout exceeded"`).
- **Почему лениво, а не cron**: соответствует существующему паттерну таймаута листа
  ожидания (`OfferTTL`, ленивая проверка в `Confirm`); не вводит фоновых воркеров.

### Решение 6: Приоритет отмены — расширить предикат отмены
`CancelAndOfferWaitlist` (и `Cancel`/`ForceCancel`) сейчас отменяют
`WHERE status='confirmed'`. Расширяем до
`status IN ('confirmed','pending_approval','approved')`, чтобы владелец мог отменить
бронь на согласовании. Приоритет отмены достигается автоматически: отмена ставит
`cancelled`, после чего условный `UPDATE` в `Approve`/`Reject` не находит `pending_approval`
и возвращает ошибку.
- `Cancel` в сервисе: проверка «уже отменена» остаётся; дедлайн `CancelDeadline`
  применяется как и раньше (для pending_approval так же — отмена в пределах дедлайна).

### Решение 7: События
Добавляем типы `TypeBookingPendingApproval = "booking.pending_approval"`,
`TypeBookingApproved = "booking.approved"`, `TypeBookingRejected = "booking.rejected"`.
Публикуем после коммита через существующий `publishBookingEvent` (несёт только id брони/
пользователя/комнаты; причина отказа в тело события не кладётся — consumer отдельный
change). Сбой публикации — Warn, операция успешна.

### Решение 8: Авторизация admin-only в service
`ListPendingApprovals`/`Approve`/`Reject` проверяют `a.IsAdmin()` в сервисе (как
`ForceCancel`), возвращая `ErrForbidden` (403 `FORBIDDEN`). Handler не авторизует.
Роуты `/admin/approvals*` регистрируются в общей authenticated-группе роутера.

### Решение 9: Отклонение освобождает слот и предлагает его листу ожидания
`rejected`-бронь перестаёт удерживать слот (Решение 2), поэтому reject освобождает
интервал — и, как отмена, предлагает освободившийся слот первому подходящему в листе
ожидания. Reject идёт через `RejectAndOfferWaitlist`, который в одной транзакции переводит
бронь в `rejected` и вызывает тот же `offerNextSlot`, что и `CancelAndOfferWaitlist`.
Атомарность: слот не перехватывается между reject и предложением. Авто-reject по таймауту
(Решение 5) использует тот же путь.
- **Изменение к первой версии**: изначально предполагалось, что reject не трогает waitlist
  (снятый Open Question). Требование уточнено: раз слот освобождается, очередь должна
  получить его так же, как при отмене.
- Сервис после reject сбрасывает кэш доступности (слот свободен) и логирует предложенную
  запись, если она есть (как `Cancel`).

### Новые доменные ошибки
`service/errors.go`: `ErrApprovalNotFound` (404 `APPROVAL_NOT_FOUND`),
`ErrNotPendingApproval` (409 `NOT_PENDING_APPROVAL`). Отказ по роли переиспользует
существующую `ErrForbidden`; валидация пустого `reason` — существующая `ValidationError`.
Парная ветка в `handler/errors.go`.

## Risks / Trade-offs

- **`ALTER TYPE ... ADD VALUE` и транзакции** → миграция только добавляет значения enum и
  колонки, но не использует новые значения в том же файле; на PG 12+ это безопасно.
  Использовать `ADD VALUE IF NOT EXISTS`. Если применение обёрнуто в один
  `BEGIN/COMMIT` — убедиться, что новые значения не читаются/пишутся в той же транзакции.
- **Down-миграция не удаляет значения enum** → PostgreSQL не умеет `DROP VALUE`. `down.sql`
  снимает добавленные колонки; лишние значения enum остаются неиспользуемыми. Задокументировать.
- **Расширение предиката занятости ломает существующие тесты/ожидания** → все места с
  `status='confirmed'` для проверки занятости обновляются согласованно; unit- и
  интеграционные тесты на пересечения дополняются кейсами `pending_approval`/`approved`.
- **`created_at` для существующих строк** → бэкфилл `DEFAULT now()` присвоит старым броням
  время миграции. Приемлемо: таймаут касается только `pending_approval`, а такие брони все
  новые (после релиза).
- **Backward-compat 202** → клиент, жёстко проверяющий `== 201`, сломается на больших
  комнатах. Помечено BREAKING; фиксируется в openapi.yaml.
- **Связанность с листом ожидания** → теперь можно встать в очередь на `pending_approval`
  слот. При его отклонении (в т.ч. авто-reject по таймауту) слот освобождается и
  предлагается очереди тем же механизмом, что и при отмене (`offerNextSlot`, Решение 9).

## Migration Plan

1. Добавить `migrations/004_booking_approval.up.sql`:
   `ALTER TYPE booking_status ADD VALUE IF NOT EXISTS 'pending_approval'`,
   `... 'approved'`, `... 'rejected'`; `ALTER TABLE bookings ADD COLUMN rejection_reason
   TEXT`, `ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now()`; индекс для выборки
   ожидающих: `CREATE INDEX idx_bookings_pending_created ON bookings (status, created_at)
   WHERE status = 'pending_approval'`.
2. `004_booking_approval.down.sql`: `DROP INDEX`, `ALTER TABLE bookings DROP COLUMN ...`
   (значения enum остаются — задокументировано).
3. Деплой: миграция обратно совместима со старым кодом (новые значения/колонки не
   используются, пока не выкачен новый код). Rollback: откатить код, затем `down.sql`.

## Open Questions

- Нужно ли хранить/возвращать идентификатор одобрившего/отклонившего админа и `created_at`
  в API? Пока нет: `CreatedAt` — внутренний якорь таймаута (не в JSON), наружу в брони
  отдаём только `rejection_reason`.
- Нужен ли отдельный `GET /bookings/{id}` владельцу, чтобы увидеть причину отказа? Сейчас
  причина возвращается в ответе на reject и доступна через списки по пользователю; отдельный
  эндпоинт вне scope.
