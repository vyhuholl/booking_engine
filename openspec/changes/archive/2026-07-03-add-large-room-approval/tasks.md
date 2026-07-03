## 1. Миграция и модель

<!-- Миграция расщеплена на две пары (004 значения enum, 005 колонки+индекс): PG
     запрещает использовать только что добавленное значение enum в той же
     транзакции, а партиальный индекс ссылается на 'pending_approval'. Проверено:
     up 001..005 и down 005 применяются на postgres:16 через migrate/migrate. -->

- [x] 1.1 Создать `migrations/004_booking_status_values.up.sql`: `ALTER TYPE booking_status ADD VALUE IF NOT EXISTS 'pending_approval' / 'approved' / 'rejected'` (отдельной миграцией — значения нужно закоммитить до использования)
- [x] 1.2 Создать `migrations/005_booking_approval_columns.up.sql`: `ALTER TABLE bookings ADD COLUMN rejection_reason TEXT`, `ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now()`
- [x] 1.3 В `005`: частичный индекс `idx_bookings_pending_created ON bookings (status, created_at) WHERE status = 'pending_approval'` (отдельно от ADD VALUE — ссылается на новое значение)
- [x] 1.4 down-миграции: `005_booking_approval_columns.down.sql` (`DROP INDEX`, `DROP COLUMN created_at`, `DROP COLUMN rejection_reason`) и `004_booking_status_values.down.sql` (заглушка-комментарий: значения enum PostgreSQL не удаляет)
- [x] 1.5 Добавить константы статусов в `internal/model/model.go`: `StatusPendingApproval`, `StatusApproved`, `StatusRejected`
- [x] 1.6 Добавить в `model.Booking` поля `RejectionReason *string` (json `rejection_reason,omitempty`) и `CreatedAt time.Time` (якорь таймаута, заполняется репозиторием из колонки `created_at`; в JSON `-`)
- [x] 1.7 Ввести хелпер/набор активных (занимающих слот) статусов: `model.ActiveBookingStatuses` + метод `BookingStatus.IsActive()` (`confirmed`/`pending_approval`/`approved`) — единый источник для предикатов занятости

## 2. Расширение предиката занятости (repository)

<!-- Предикат централизован: activeStatusList в booking.go собирается из
     model.ActiveBookingStatuses (единый источник). Помимо методов из 2.1/2.2/2.4
     предикат расширен ЕЩЁ в 4 местах (по scope из proposal — «проверка пересечений,
     доступность комнат, лист ожидания») — на ревью:
       - booking.go HasActiveForRoom (страховка удаления комнаты)
       - room.go Available (доступность комнат)
       - waitlist.go Create busy-check и ConfirmAndBook conflict-check (лист ожидания)
     НЕ трогали ListByRoomOnDate/InPeriod/CountByRoomInPeriod — они намеренно учитывают
     любой статус (картина использования). -->

- [x] 2.1 В `internal/repository/booking.go` заменить `status = 'confirmed'` на `status IN (activeStatusList)` в `CreateChecked`
- [x] 2.2 То же в `IsRoomBusy` и `ListConflicting`
- [x] 2.3 Обновить `Get`/`scanBookings` (и все SELECT/RETURNING через `bookingColumns`), чтобы сканировать `rejection_reason` и `created_at` в `model.Booking`
- [x] 2.4 Расширить предикат отмены до `status IN (activeStatusList)` в `Cancel` и `CancelAndOfferWaitlist`
- [x] 2.5 Интеграционный тест `TestBooking_OccupancyPredicate`: `confirmed`/`pending_approval`/`approved` бронь делает слот занятым (конфликт `CreateChecked`, `true` в `IsRoomBusy`); `rejected`/`cancelled` — освобождает

## 3. Репозиторий: approve / reject / list / timeout

- [x] 3.1 Добавить `ListPendingApprovals(ctx, now, timeout, reason) ([]Booking pending, []Booking autoRejected, error)` — в Serializable-транзакции сначала «подмести» просроченные (`created_at < now-timeout`) через общий `rejectPending` (rejected + причина + предложение слота waitlist), собрать их в `autoRejected` для событий, затем вернуть оставшиеся `pending_approval` (по `created_at`)
- [x] 3.2 Добавить `Approve(ctx, id, now) (model.Booking, error)` — условный `UPDATE ... SET status='approved' WHERE id=$1 AND status='pending_approval' RETURNING ...`; 0 строк → `ErrNotFound` (сигнал «не pending»). Таймаут в SQL не проверяем — это делает сервис по `CreatedAt` (см. 5.6)
- [x] 3.3 Добавить `RejectAndOfferWaitlist(ctx, id, reason, now) (model.Booking, *model.WaitlistEntry, error)` — Serializable, через общий `rejectPending`: условный `UPDATE ... SET status='rejected', rejection_reason=$reason WHERE id=$1 AND status='pending_approval' RETURNING ...` + предложение слота листу ожидания тем же `offerNextSlot`, что в `CancelAndOfferWaitlist`; 0 строк → `ErrNotFound`
- [x] 3.4 Таймаут проверяется в сервисе (по `Booking.CreatedAt`), не в SQL `Approve`/`RejectAndOfferWaitlist`; авто-reject по таймауту идёт через `rejectPending`/`RejectAndOfferWaitlist` с причиной-сентинелом (слот тоже освобождается и предлагается очереди)
- [x] 3.5 Интеграционные тесты (`approval_repository_test.go`): успешный approve/reject; reject освобождает слот и предлагает его первому в waitlist; идемпотентность (2-й вызов → `ErrNotFound`); гонка N approve (ровно один успех); авто-reject просроченных при `ListPendingApprovals` (+ предложение их слотов очереди)

## 4. События

- [x] 4.1 Добавить в `internal/events/events.go` типы `TypeBookingPendingApproval`, `TypeBookingApproved`, `TypeBookingRejected`
- [x] 4.2 `publishBookingEvent` переиспользован для новых типов (несёт только id брони/пользователя/комнаты)

## 5. Сервис: доменные ошибки и логика

- [x] 5.1 Добавить в `internal/service/errors.go` sentinel `ErrApprovalNotFound`, `ErrNotPendingApproval`
- [x] 5.2 Добавить константы `LargeRoomCapacityThreshold = 12`, `ApprovalTimeout = 24 * time.Hour`, `ApprovalTimeoutReason`
- [x] 5.3 Расширить `BookingRepo`-интерфейс методами `Approve`, `RejectAndOfferWaitlist`, `ListPendingApprovals` (+ mock-заглушка `ListPendingApprovals` в тестах, чтобы `mockBookingRepo` удовлетворял интерфейсу)
- [x] 5.4 В `Booking.Create` выбирать статус по `room.Capacity > LargeRoomCapacityThreshold` (`pending_approval` иначе `confirmed`); публиковать `booking.pending_approval` для больших комнат, `booking.created` для малых
- [x] 5.5 Реализовать `Booking.ListPendingApprovals(ctx, a)` — admin-only (`ErrForbidden`), публиковать `booking.rejected` для авто-отклонённых
- [x] 5.6 Реализовать `Booking.Approve(ctx, a, id)` — admin-only; `Get`→404 `ErrApprovalNotFound`; статус ≠ `pending_approval` → `ErrNotPendingApproval`; если `now - CreatedAt > ApprovalTimeout` → авто-reject через `RejectAndOfferWaitlist` (событие `booking.rejected`) + `ErrNotPendingApproval`; иначе `Approve` в репо (0 строк → `ErrNotPendingApproval`), событие `booking.approved`; кэш не трогаем (слот уже был занят)
- [x] 5.7 Реализовать `Booking.Reject(ctx, a, id, reason)` — admin-only; валидация непустого `reason` (`ValidationError`); `Get`→404 `ErrApprovalNotFound`; статус ≠ `pending_approval` → `ErrNotPendingApproval`; `RejectAndOfferWaitlist` (0 строк → `ErrNotPendingApproval`); событие `booking.rejected`; сброс кэша доступности + лог предложенной waitlist-записи (хелпер `logOffered`)
- [x] 5.8 Отмена `pending_approval`/`approved` в `Cancel` работает без изменений сервиса — включена расширением предиката отмены в репозитории (секция 2); покрыто unit-тестом `TestBookingService_Cancel_PendingApproval`

## 6. Handler и роутинг

- [x] 6.1 В `internal/handler/booking.go` вернуть 202 при `booking.Status == pending_approval`, иначе 201 в `createBooking`
- [x] 6.2 Добавить хендлеры `listApprovals`, `approveBooking`, `rejectBooking` (тело `{ "reason": "..." }`) в `internal/handler/approval.go` — вызывают сервис, авторизация внутри сервиса
- [x] 6.3 Зарегистрировать роуты в `handler.go`: `GET /admin/approvals`, `POST /admin/approvals/{id}/approve`, `POST /admin/approvals/{id}/reject` в authenticated-группе
- [x] 6.4 Добавить ветки в `internal/handler/errors.go`: `ErrApprovalNotFound` → 404 `APPROVAL_NOT_FOUND`, `ErrNotPendingApproval` → 409 `NOT_PENDING_APPROVAL`
- [x] 6.5 Handler-тесты (`approval_test.go`): 202/201 по вместимости; list/approve/reject happy path; 403 не-админу; 404 `APPROVAL_NOT_FOUND`; 409 `NOT_PENDING_APPROVAL`; 400 на пустой reason / битый JSON

## 7. API-спека и проверка

- [x] 7.1 Обновить `api/openapi.yaml`: код 202 для `POST /bookings`, новые статусы в `BookingStatus`, поле `rejection_reason` в `Booking`, схема `ApprovalReject`, эндпоинты `/admin/approvals*`, тег `Approvals`, коды `APPROVAL_NOT_FOUND`/`NOT_PENDING_APPROVAL`
- [x] 7.2 Прогнать `go test ./...`: `service`/`repository`/`handler` и остальные реализованные пакеты — PASS (`internal/notifications` — отдельный парный change, не в scope)
- [x] 7.3 `openspec validate --changes add-large-room-approval` — passed
