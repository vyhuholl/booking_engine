## 1. Миграция и модель

- [ ] 1.1 Создать `migrations/004_booking_approval.up.sql`: `ALTER TYPE booking_status ADD VALUE IF NOT EXISTS 'pending_approval' / 'approved' / 'rejected'`
- [ ] 1.2 В той же миграции: `ALTER TABLE bookings ADD COLUMN rejection_reason TEXT`, `ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now()`
- [ ] 1.3 Добавить частичный индекс `idx_bookings_pending_created ON bookings (status, created_at) WHERE status = 'pending_approval'`
- [ ] 1.4 Создать `migrations/004_booking_approval.down.sql`: `DROP INDEX`, `DROP COLUMN rejection_reason`, `DROP COLUMN created_at` (значения enum остаются — задокументировать комментарием)
- [ ] 1.5 Добавить константы статусов в `internal/model/model.go`: `StatusPendingApproval`, `StatusApproved`, `StatusRejected`
- [ ] 1.6 Добавить поле `RejectionReason *string` (json `rejection_reason,omitempty`) в `model.Booking`
- [ ] 1.7 Ввести хелпер/набор активных (занимающих слот) статусов: `confirmed`, `pending_approval`, `approved` (использовать в предикатах занятости)

## 2. Расширение предиката занятости (repository)

- [ ] 2.1 В `internal/repository/booking.go` заменить `status = 'confirmed'` на `status IN ('confirmed','pending_approval','approved')` в `CreateChecked`
- [ ] 2.2 То же в `IsRoomBusy` и `ListConflicting`
- [ ] 2.3 Обновить `Get`/`scanBookings` (и все SELECT-列表), чтобы сканировать `rejection_reason` в `model.Booking`
- [ ] 2.4 Расширить предикат отмены до `status IN ('confirmed','pending_approval','approved')` в `Cancel` и `CancelAndOfferWaitlist`
- [ ] 2.5 Интеграционный тест: `pending_approval`/`approved` бронь делает слот занятым (409 при `CreateChecked`, `true` в `IsRoomBusy`); `rejected`/`cancelled` — освобождает

## 3. Репозиторий: approve / reject / list / timeout

- [ ] 3.1 Добавить `ListPendingApprovals(ctx, now)` — в Serializable-транзакции сначала авто-reject просроченных (`status='pending_approval' AND created_at < now-24h` → `rejected` + причина-сентинел, вернуть их id для событий), затем вернуть оставшиеся `pending_approval`
- [ ] 3.2 Добавить `Approve(ctx, id, now)` — условный `UPDATE ... SET status='approved' WHERE id=$1 AND status='pending_approval' AND created_at >= now-24h RETURNING ...`; 0 строк → `ErrNotFound`-эквивалент/сигнал «не pending»
- [ ] 3.3 Добавить `Reject(ctx, id, reason, now)` — условный `UPDATE ... SET status='rejected', rejection_reason=$reason WHERE ... status='pending_approval' AND created_at >= now-24h RETURNING ...`
- [ ] 3.4 В `Approve`/`Reject` обработать просроченную бронь: перевести в `rejected` (auto) и вернуть сигнал «не pending», чтобы сервис отдал `NOT_PENDING_APPROVAL`
- [ ] 3.5 Интеграционные тесты: успешный approve/reject; повторный (идемпотентность → 0 строк); гонка двух approve (ровно один успех); авто-reject по таймауту при list и при approve/reject

## 4. События

- [ ] 4.1 Добавить в `internal/events/events.go` типы `TypeBookingPendingApproval`, `TypeBookingApproved`, `TypeBookingRejected`
- [ ] 4.2 Убедиться, что `publishBookingEvent` переиспользуется для новых типов (несёт только id)

## 5. Сервис: доменные ошибки и логика

- [ ] 5.1 Добавить в `internal/service/errors.go` sentinel `ErrApprovalNotFound`, `ErrNotPendingApproval`
- [ ] 5.2 Добавить константы `LargeRoomCapacityThreshold = 12`, `ApprovalTimeout = 24 * time.Hour`, причину авто-reject
- [ ] 5.3 Расширить `BookingRepo`-интерфейс в service методами `ListPendingApprovals`, `Approve`, `Reject`
- [ ] 5.4 В `Booking.Create` выбирать статус по `room.Capacity > 12` (`pending_approval` иначе `confirmed`); публиковать `booking.pending_approval` для больших комнат, `booking.created` для малых
- [ ] 5.5 Реализовать `Booking.ListPendingApprovals(ctx, a)` — admin-only (`ErrForbidden`), публиковать `booking.rejected` для авто-отклонённых
- [ ] 5.6 Реализовать `Booking.Approve(ctx, a, id)` — admin-only; `Get`→404 `ErrApprovalNotFound`; условный апдейт; 0 строк → `ErrNotPendingApproval`; событие `booking.approved`; сброс кэша доступности не нужен (слот уже был занят)
- [ ] 5.7 Реализовать `Booking.Reject(ctx, a, id, reason)` — admin-only; валидация непустого `reason` (`ValidationError`); условный апдейт; событие `booking.rejected`; сброс кэша доступности (слот освободился)
- [ ] 5.8 Разрешить отмену `pending_approval`/`approved` в `Cancel` (проверка «уже отменена», дедлайн — как есть)
- [ ] 5.9 Unit-тесты (моки-структуры, `testutil`): создание большой/малой комнаты (статус+событие); approve/reject happy path; повторный approve/reject → `ErrNotPendingApproval`; approve/reject не-админом → `ErrForbidden`; reject без причины → `ValidationError`; таймаут; отмена pending_approval + затем approve → ошибка

## 6. Handler и роутинг

- [ ] 6.1 В `internal/handler/booking.go` вернуть 202 при `booking.Status == pending_approval`, иначе 201 в `createBooking`
- [ ] 6.2 Добавить хендлеры `listApprovals`, `approveBooking`, `rejectBooking` (тело `{ "reason": "..." }`) — вызывают сервис, авторизация внутри сервиса
- [ ] 6.3 Зарегистрировать роуты в `handler.go`: `GET /admin/approvals`, `POST /admin/approvals/{id}/approve`, `POST /admin/approvals/{id}/reject` в authenticated-группе
- [ ] 6.4 Добавить ветки в `internal/handler/errors.go`: `ErrApprovalNotFound` → 404 `APPROVAL_NOT_FOUND`, `ErrNotPendingApproval` → 409 `NOT_PENDING_APPROVAL`

## 7. API-спека и проверка

- [ ] 7.1 Обновить `api/openapi.yaml`: код 202 для `POST /bookings`, новые статусы, поле `rejection_reason`, эндпоинты `/admin/approvals*`, тело reject, коды `APPROVAL_NOT_FOUND`/`NOT_PENDING_APPROVAL`
- [ ] 7.2 Прогнать `go test ./internal/service/...` (unit) и `go test ./internal/repository/...` (нужен Docker)
- [ ] 7.3 Прогнать `openspec validate --changes add-large-room-approval` (или через store) и убедиться в отсутствии ошибок
