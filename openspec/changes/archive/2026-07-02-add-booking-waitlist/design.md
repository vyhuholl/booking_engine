## Context

Движок бронирования — слоистый REST-сервис (handler → service → repository), Go 1.25, PostgreSQL
через `pgx/v5`, сырой SQL. Отмена/создание брони уже защищены от гонок: `CreateChecked` работает
в транзакции уровня Serializable и без ретраев (см. `internal/repository/booking.go` и тест
`TestBookingService_Create_ConcurrentDoubleBooking`). Отмена сейчас — одиночный
`UPDATE ... WHERE id=$1 AND status='confirmed'`.

Waitlist добавляет вторую доменную сущность рядом с бронью, и её ключевые операции пересекают
границу двух таблиц (`bookings` ↔ `waitlist_entries`) под конкурентной нагрузкой: отмена должна
атомарно предложить слот, а подтверждение — атомарно создать бронь и «погасить» запись. Это
основной источник сложности и причина отдельного design-документа.

## Goals / Non-Goals

**Goals:**
- Waitlist-запись создаётся только на занятый интервал; свободный интервал → обычная бронь.
- Автоназначаемая, нередактируемая `position`; FIFO-предложение слота по возрастанию `position`.
- Атомарность двух критичных операций: (1) отмена брони + предложение слота, (2) подтверждение
  слота = создание брони + перевод записи в `converted`.
- Соблюдение слоистости: SQL и транзакции — только в repository; правила и авторизация — в service;
  handler только разбирает запрос и мапит ошибки.
- Ленивая проверка 15-минутного таймаута `offered` при `confirm`.

**Non-Goals:**
- Уведомления (email/push) о предложенном слоте.
- Фоновый воркер/cron для протухания `offered`-записей — протухание только лениво при `confirm`.
- Приоритеты/веса в очереди, кроме FIFO по `position`.
- Кэширование очереди в Redis и публикация отдельных waitlist-событий в Kafka (при конвертации
  переиспользуем существующее `booking.created`; отдельные типы событий — за рамками).

## Decisions

### Модель данных: таблица `waitlist_entries`
Новая таблица (миграция `003_waitlist`):
```
id          TEXT PRIMARY KEY            -- 'wl-<uuid>'
room_id     TEXT NOT NULL REFERENCES rooms(id)
user_id     TEXT NOT NULL REFERENCES users(id)
start_time  TIMESTAMPTZ NOT NULL
end_time    TIMESTAMPTZ NOT NULL
position    INTEGER NOT NULL
status      waitlist_status NOT NULL DEFAULT 'waiting'
offered_at  TIMESTAMPTZ                 -- NULL, кроме статуса 'offered'
created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
CHECK (end_time > start_time)
```
Новый enum `waitlist_status AS ENUM ('waiting','offered','expired','converted')`.

Инвариант уникальности через **частичный уникальный индекс** (активные записи):
```
CREATE UNIQUE INDEX uq_waitlist_active
  ON waitlist_entries (room_id, user_id, start_time, end_time)
  WHERE status IN ('waiting','offered');
```
Так пользователь после `expired`/`converted`/выхода может встать в очередь заново, но не может
иметь два активных места на один интервал.

Индекс для выборки очереди и предложения слота:
```
CREATE INDEX idx_waitlist_room_status_pos
  ON waitlist_entries (room_id, status, position);
```

*Альтернатива (отклонена):* хранить `position` вычисляемым «на лету» через `ROW_NUMBER()`.
Отклонено: позиция должна быть стабильным ординалом, показываемым в API и не «прыгать» при выходе
других из очереди.

### Семантика `position`
`position` назначается **в масштабе комнаты** как `COALESCE(MAX(position),0)+1` среди активных
(`waiting`/`offered`) записей комнаты, вычисляется внутри транзакции создания. FIFO по комнате даёт
однозначный порядок при предложении слота на пересекающиеся интервалы. Позиции не перенумеровываются
при выходе/протухании — `GET` отдаёт сохранённый ординал, сортировка по `position` остаётся корректной.

*Альтернатива (отклонена):* позиция в масштабе конкретного интервала. Отклонено: интервалы у записей
разные, а слот предлагается по пересечению — сквозной FIFO по комнате проще и предсказуемее.

### Атомарность: транзакции живут в repository
Обе кросс-табличные операции инкапсулированы в методах repository (Serializable, без ретраев —
как `CreateChecked`), service лишь оркеструет и принимает решение вызвать их:

1. **Отмена + предложение** — новый метод `repository.Booking.CancelAndOfferWaitlist(ctx, bookingID, now)`:
   в одной транзакции (a) `UPDATE bookings SET status='cancelled' WHERE id=$1 AND status='confirmed'`
   (0 строк → `ErrNotFound`), (b) `SELECT ... FROM waitlist_entries WHERE room_id=$roomID AND
   status='waiting' AND start_time < $cancelledEnd AND end_time > $cancelledStart ORDER BY position
   LIMIT 1 FOR UPDATE SKIP LOCKED`, (c) если найдена — `UPDATE ... SET status='offered',
   offered_at=$now`. Возвращает отменённую бронь и опционально предложенную запись.
   Метод спанит две таблицы — это допустимо: repository по-прежнему только читает/пишет,
   решение «предлагать при отмене» уже принято в service.

2. **Подтверждение** — новый метод `repository.Waitlist.ConfirmAndBook(ctx, entry, booking)`:
   в одной транзакции (a) `UPDATE waitlist_entries SET status='converted' WHERE id=$1 AND
   status='offered'` (0 строк → `ErrNotFound`/погашено гонкой), (b) проверка пересечений и
   `INSERT INTO bookings` (как в `CreateChecked`). Условный UPDATE — точка сериализации: из двух
   одновременных `confirm` ровно один увидит `status='offered'` и продолжит.

3. **Протухание при confirm** — `repository.Waitlist.ExpireAndOfferNext(ctx, entryID, now)`:
   в одной транзакции переводит запись в `expired` и предлагает следующую подходящую `waiting`-запись.

Служебные методы repository: `Create` (атомарно вычисляет `position` + вставка, ловит нарушение
`uq_waitlist_active` → `ErrConflict`), `Get`, `ListByRoom`, `Delete`.

*Альтернатива (отклонена):* оркестрировать транзакцию в service, прокидывая `pgx.Tx`. Отклонено:
нарушает правило «никакого pgx в service».

### Служба `service.Waitlist`
Новый файл `internal/service/waitlist.go` по образцу `service.Booking`: конструктор `NewWaitlist(...)`
с инъекцией `RoomLookup`, репозиториев waitlist и booking, логгера и `now func() time.Time`. Интерфейсы
потребителя (`WaitlistRepo`, при необходимости расширение `BookingRepo`) объявляются в service.
Методы: `Join`, `ListByRoom`, `Confirm`, `Leave`. Валидация интервала переиспользует те же правила и
ошибки, что `Booking.Create` (общий приватный хелпер валидации интервала, чтобы не дублировать).

Проверка «комната занята» перед постановкой: `RoomLookup`/`BookingRepo` уже умеют искать пересечения
(`CreateChecked` делает это внутри); для waitlist добавим явную проверку занятости через существующий
запрос пересечения (например, метод-предикат в booking-репозитории). Если свободно — `ErrRoomAvailable`.

Константа `OfferTTL = 15 * time.Minute` в `service` рядом с `CancelDeadline`.

### Интеграция с `Booking.Cancel`
`Booking.Cancel` заменяет вызов `bookings.Cancel(ctx, id)` на `bookings.CancelAndOfferWaitlist(ctx, id, s.now())`.
Логирование/инвалидация кэша/публикация события сохраняются. Если предложена запись — добавляется
Info-лог `waitlist slot offered` с `waitlist_id`/`user_id` предложенного.

### Ошибки и HTTP-маппинг
Новые sentinel в `service/errors.go` и ветки в `handler/errors.go`:
| Ошибка | HTTP | Код |
| --- | --- | --- |
| `ErrRoomAvailable` | 409 | `WAITLIST_ROOM_AVAILABLE` |
| `ErrAlreadyInWaitlist` | 409 | `ALREADY_IN_WAITLIST` |
| `ErrWaitlistNotFound` | 404 | `WAITLIST_NOT_FOUND` |
| `ErrOfferNotPending` | 409 | `OFFER_NOT_PENDING` |
| `ErrOfferExpired` | 409 | `OFFER_EXPIRED` |
| `ErrWaitlistForbidden` | 403 | `WAITLIST_FORBIDDEN` |
Переиспользуются `ErrRoomNotFound`, `ErrRoomOutOfService`, `ErrInvalidTimeRange`, `ErrStartInPast`,
`ErrDurationTooShort/Long`, `BookingConflictError`.

### Handler и роутинг
`internal/handler/waitlist.go`: `joinWaitlist`, `listWaitlist`, `confirmWaitlist`, `leaveWaitlist`.
В `Router()`: `POST/GET /rooms/{id}/waitlist` в блок `/rooms`; новый блок `/waitlist` с
`POST /{id}/confirm` и `DELETE /{id}`. `Handler` получает поле `waitlist *service.Waitlist`,
конструктор `New(...)` и wiring в `cmd/server/main.go` расширяются.

## Risks / Trade-offs

- **[Кросс-табличный метод в booking-репозитории размывает границы сущностей]** → Инкапсулируем в
  одном методе с ясным именем `CancelAndOfferWaitlist`; решение о предложении принимает service.
  Альтернатива с `pgx.Tx` в service хуже (протечка слоя).
- **[Serializable + отсутствие ретраев → часть confirm/cancel под гонкой падают с 40001]** →
  Это осознанный контракт (как у `CreateChecked`): проигравший получает ошибку, инвариант «ровно
  одна бронь» держится. Тесты покрывают обе ветки (условный UPDATE и serialization_failure).
- **[Позиции не перенумеровываются при выходе из очереди]** → Приемлемо: `position` — стабильный
  ординал для сортировки/предложения, «дыры» в нумерации не влияют на FIFO. Документируется в API.
- **[Ленивое протухание: `offered`-слот держит очередь до чьего-либо `confirm`]** → Осознанное
  Non-Goal (нет cron). Следующий кандидат получает слот при ближайшем `confirm` протухшей записи.
  Живая проблема только без активности; допустимо для текущего объёма.
- **[`FOR UPDATE SKIP LOCKED` при предложении]** → Гарантирует, что параллельные отмены не предложат
  одну запись дважды; проверяется тестом конкурентности.
- **[Occupied-check и последующая отмена брони — TOCTOU]** → Комната могла освободиться между
  проверкой занятости и созданием waitlist. Приемлемо: запись остаётся `waiting`, её слот просто
  никогда не «предложится» отменой (брони уже нет); пользователь может выйти. Строгой атомарности
  тут не требуется по спеке.

## Migration Plan

1. Добавить пару миграций `003_waitlist.up.sql` / `.down.sql` (enum, таблица, индексы). `up`
   аддитивна — существующие данные не трогает; применяется по возрастанию номера (см. CLAUDE.md).
2. Выкатить код (модель, repository, service, handler, wiring). Обратная совместимость: старые
   эндпоинты и брони не меняются; у `cancel` появляется дополнительный побочный эффект.
3. **Rollback**: `003_waitlist.down.sql` (DROP TABLE + DROP TYPE) и откат кода. Так как waitlist
   аддитивен и не меняет схему `bookings`, откат безопасен; незавершённые `offered`-записи просто
   исчезают вместе с таблицей.

## Open Questions

- Нужно ли admin/manager видеть в `GET /rooms/{id}/waitlist` `user_id` всех записей, или только
  агрегированные позиции для не-владельцев? Спека сейчас отдаёт `user_id` всем аутентифицированным —
  оставляем так, если приватность не потребуется.
- Публиковать ли отдельные waitlist-события (`waitlist.offered`, `waitlist.converted`) для будущих
  уведомлений? Пока переиспользуем `booking.created` при конвертации; отдельные типы — отдельным
  изменением, если появится потребитель.
