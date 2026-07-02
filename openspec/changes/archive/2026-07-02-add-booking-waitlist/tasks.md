## 1. Модель и миграции

- [x] 1.1 Добавить в `internal/model/model.go` тип `WaitlistStatus` с константами `WaitlistStatusWaiting`/`Offered`/`Expired`/`Converted` и методом `Valid()`
- [x] 1.2 Добавить структуру `WaitlistEntry` (ID, RoomID, UserID, StartTime, EndTime, Position, Status, OfferedAt `*time.Time`, CreatedAt) с JSON-тегами по конвенции
- [x] 1.3 Создать `migrations/003_waitlist.up.sql`: enum `waitlist_status`, таблица `waitlist_entries` (PK `wl-<uuid>`, FK на rooms/users, timestamptz, CHECK end>start), частичный уникальный индекс `uq_waitlist_active` на (room_id,user_id,start_time,end_time) WHERE status IN ('waiting','offered'), индекс `idx_waitlist_room_status_pos`
- [x] 1.4 Создать `migrations/003_waitlist.down.sql` (DROP TABLE, DROP TYPE)

## 2. Репозиторий (internal/repository/waitlist.go)

- [x] 2.1 Создать тип `repository.Waitlist` с конструктором `NewWaitlist(pool)`
- [x] 2.2 `Create(ctx, entry) (model.WaitlistEntry, error)` — в транзакции вычислить `position = COALESCE(MAX(position),0)+1` среди активных записей комнаты и вставить; нарушение `uq_waitlist_active` (pgconn.PgError 23505) → доменная `ErrConflict`
- [x] 2.3 `Get(ctx, id) (model.WaitlistEntry, error)` — с нормализацией `pgx.ErrNoRows` → `ErrNotFound` (`wrapNoRows`)
- [x] 2.4 `ListByRoom(ctx, roomID) ([]model.WaitlistEntry, error)` — ORDER BY position ASC, пустой срез (не nil)
- [x] 2.5 `Delete(ctx, id) error` — 0 строк → `ErrNotFound`
- [x] 2.6 `ConfirmAndBook(ctx, entry, booking) (conflict *model.Booking, err error)` — Serializable tx: условный `UPDATE ... SET status='converted' WHERE id=$1 AND status='offered'` (0 строк → `ErrNotFound`), затем проверка пересечений + INSERT брони (как в `CreateChecked`)
- [x] 2.7 `ExpireAndOfferNext(ctx, entryID, now) (offered *model.WaitlistEntry, err error)` — Serializable tx: `UPDATE ... status='expired'`, затем найти и предложить следующую подходящую `waiting`-запись (ORDER BY position, `FOR UPDATE SKIP LOCKED`)
- [x] 2.8 Добавить в `repository.Booking` метод `CancelAndOfferWaitlist(ctx, bookingID, now) (booking model.Booking, offered *model.WaitlistEntry, err error)` — Serializable tx: отмена брони (0 строк → `ErrNotFound`) + предложение первой пересекающейся `waiting`-записи (`FOR UPDATE SKIP LOCKED`)
- [x] 2.9 Добавить в `repository.Booking` метод-предикат занятости комнаты в интервале `IsRoomBusy(ctx, roomID, start, end) (bool, error)` (SELECT EXISTS пересечения по status='confirmed')

## 3. Сервис (internal/service/waitlist.go)

- [x] 3.1 Добавить sentinel-ошибки в `internal/service/errors.go`: `ErrRoomAvailable`, `ErrAlreadyInWaitlist`, `ErrWaitlistNotFound`, `ErrOfferNotPending`, `ErrOfferExpired`, `ErrWaitlistForbidden`
- [x] 3.2 Вынести общую валидацию интервала брони (future/end>start/длительность) в приватный хелпер, переиспользуемый `Booking.Create` и waitlist `Join` (без дублирования правил и ошибок)
- [x] 3.3 Объявить в service интерфейс `WaitlistRepo` (Create/Get/ListByRoom/Delete/ConfirmAndBook/ExpireAndOfferNext) и расширить `BookingRepo` методами `CancelAndOfferWaitlist`, `IsRoomBusy`
- [x] 3.4 Создать тип `service.Waitlist` + `NewWaitlist(rooms, waitlist, bookings, log)` с полем `now func() time.Time` и константой `OfferTTL = 15*time.Minute`
- [x] 3.5 `Join(ctx, a, WaitlistJoinInput)` — валидация интервала, проверка комнаты (существует/не out_of_service), `IsRoomBusy` (свободна → `ErrRoomAvailable`), создание записи (дубликат репо → `ErrAlreadyInWaitlist`); структурный лог
- [x] 3.6 `ListByRoom(ctx, a, roomID)` — вернуть очередь (по position)
- [x] 3.7 `Confirm(ctx, a, id)` — Get+ownership (`ErrWaitlistForbidden`), статус != offered → `ErrOfferNotPending`; таймаут (now-offered_at > OfferTTL) → `ExpireAndOfferNext` + `ErrOfferExpired`; иначе собрать `model.Booking` и `ConfirmAndBook` (conflict → `BookingConflictError`); лог + инвалидация кэша комнаты
- [x] 3.8 `Leave(ctx, a, id)` — Get+ownership (владелец или admin), `Delete`
- [x] 3.9 Изменить `Booking.Cancel`: вместо `bookings.Cancel` вызвать `bookings.CancelAndOfferWaitlist(ctx, id, s.now())`; при предложении — Info-лог `waitlist slot offered` с `waitlist_id`

## 4. Handler (internal/handler)

- [x] 4.1 Создать `internal/handler/waitlist.go`: `joinWaitlist` (POST /rooms/{id}/waitlist), `listWaitlist` (GET /rooms/{id}/waitlist), `confirmWaitlist` (POST /waitlist/{id}/confirm → 201), `leaveWaitlist` (DELETE /waitlist/{id} → 204); тела `waitlistJoinBody` (unexported)
- [x] 4.2 Добавить в `handler/errors.go` ветки маппинга новых ошибок на HTTP-коды (409 `WAITLIST_ROOM_AVAILABLE`/`ALREADY_IN_WAITLIST`/`OFFER_NOT_PENDING`/`OFFER_EXPIRED`, 404 `WAITLIST_NOT_FOUND`, 403 `WAITLIST_FORBIDDEN`)
- [x] 4.3 Добавить поле `waitlist *service.Waitlist` в `Handler`, параметр в `New(...)`, роуты в `Router()` (в блок `/rooms` и новый блок `/waitlist`)

## 5. Wiring

- [x] 5.1 В `cmd/server/main.go` собрать `repository.NewWaitlist(pool)`, `service.NewWaitlist(...)` и передать в `handler.New(...)`

## 6. Тесты

- [x] 6.1 Unit-тесты `service.Waitlist` (пакет `service`, ручные моки): Join — успех/свободная комната/дубликат/невалидный интервал/out_of_service/несуществующая комната
- [x] 6.2 Unit-тесты `Confirm`: успех, чужая запись (403), не-offered (409), таймаут→expired+offer next (`ErrOfferExpired`), конфликт брони
- [x] 6.3 Unit-тесты `Leave` и `ListByRoom`; unit-тест `Booking.Cancel` на побочный эффект предложения слота (мок `CancelAndOfferWaitlist`)
- [x] 6.4 Интеграционные тесты репозитория (пакет `repository_test`, testcontainers): Create с автопозицией и уникальностью, ListByRoom сортировка, Delete, `CancelAndOfferWaitlist` предлагает первого по position
- [x] 6.5 Интеграционный тест конкурентности: два одновременных `ConfirmAndBook` одного слота — ровно один успех, в БД ровно одна бронь (по образцу `TestBookingService_Create_ConcurrentDoubleBooking`)
- [x] 6.6 Добавить хелперы в `internal/testutil` (фикстура `WaitlistEntry(opts...)` и/или `SeedWaitlist`) по образцу существующих object mothers/seed
- [x] 6.7 Handler-тесты для 4 эндпоинтов (коды ответов и маппинг ошибок), по образцу `room_list_test.go`

## 7. Документация и проверка

- [x] 7.1 Обновить `api/openapi.yaml`: 4 новых пути, схемы `WaitlistEntry`/`WaitlistJoinRequest`, новые коды ошибок; отметить новый побочный эффект у `POST /bookings/{id}/cancel`
- [x] 7.2 Прогнать `go build ./...`, `go test ./internal/service/...` и (при наличии Docker) `go test ./internal/repository/... ./internal/handler/...`; `openspec validate add-booking-waitlist`
