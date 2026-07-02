## Why

Большие переговорки (вместимость >12 человек) — дефицитный ресурс, их бронирование
нужно контролировать. Сейчас любая бронь сразу становится `confirmed` без надзора.
Нужен admin-контроль: бронь большой комнаты создаётся «на согласовании», слот при этом
резервируется, а администратор одобряет её или отклоняет с указанием причины.

## What Changes

- Вводятся новые статусы брони: `pending_approval`, `approved`, `rejected`
  (в дополнение к существующим `confirmed`, `cancelled`).
- `POST /bookings` для комнаты с `capacity > 12` создаёт бронь в статусе
  `pending_approval` и возвращает **202 Accepted** вместо 201. Малые комнаты
  (`capacity ≤ 12`) сохраняют прежнее поведение: 201 Created, статус `confirmed`.
  **BREAKING**: клиенты, жёстко ожидающие 201 на любой `POST /bookings`, должны
  научиться обрабатывать 202.
- Предикат «занятости» интервала комнаты расширяется: слот считается занятым при
  брони в статусе `confirmed`, `pending_approval` **или** `approved`. Раньше учитывался
  только `confirmed`. **BREAKING** для семантики занятости (проверка пересечений,
  доступность комнат, лист ожидания).
- Новые admin-эндпоинты:
  - `GET /admin/approvals` — список броней, ожидающих одобрения (только admin).
  - `POST /admin/approvals/{id}/approve` — одобрить (статус → `approved`).
  - `POST /admin/approvals/{id}/reject` — отклонить с причиной в теле (статус →
    `rejected`, слот освобождается, причина сохраняется).
- Причина отказа сохраняется в брони (новое поле `rejection_reason`).
- Ленивый таймаут: бронь `pending_approval` старше 24 часов автоматически
  отклоняется при обращении к ней (в списке/при approve/reject), без cron-воркера.
- Идемпотентность по статусу: повторное одобрение/отклонение уже разрешённой брони —
  ошибка. Конкурентное одобрение двумя админами — ровно один успех.
- Отмена пользователем имеет приоритет над одобрением: `pending_approval`-бронь можно
  отменить, после чего одобрение невозможно.
- При смене статуса публикуется доменное событие в Kafka:
  `booking.pending_approval` / `booking.approved` / `booking.rejected`.
  Consumer этих событий — отдельный парный change, здесь не реализуется.

## Capabilities

### New Capabilities
- `large-room-approval`: workflow одобрения броней больших переговорок — создание в
  `pending_approval` с ответом 202, резервирование слота на время рассмотрения,
  admin-эндпоинты одобрения/отклонения, сохранение причины отказа, ленивый 24-часовой
  таймаут, идемпотентность и конкурентная безопасность смены статуса, приоритет отмены,
  публикация событий смены статуса.

### Modified Capabilities
- `booking-waitlist`: определение «занятости» интервала расширяется — брони в статусе
  `pending_approval` и `approved` тоже делают комнату занятой, поэтому встать в лист
  ожидания можно и на слот, удерживаемый такой бронью (раньше — только на слот с
  `confirmed`-бронью).

## Impact

- **Модель** (`internal/model/model.go`): новые константы `BookingStatus`
  (`pending_approval`, `approved`, `rejected`); поле `RejectionReason *string` в
  `model.Booking`.
- **Миграции** (`migrations/`): новая пара `004_*` — расширение enum `booking_status`,
  колонки `rejection_reason` и `created_at` (якорь для таймаута) в таблице `bookings`.
- **События** (`internal/events/events.go`): новые типы
  `TypeBookingPendingApproval`, `TypeBookingApproved`, `TypeBookingRejected`.
- **Сервис** (`internal/service/booking.go`): ветвление `Create` по `capacity`; новые
  методы `ListPendingApprovals`, `Approve`, `Reject`; `Cancel` допускает отмену
  `pending_approval`/`approved`; общий предикат активных статусов.
- **Репозиторий** (`internal/repository/booking.go`): расширение предиката занятости в
  `CreateChecked`/`IsRoomBusy`/`ListConflicting`; новые методы одобрения/отклонения
  (Serializable, условный `UPDATE ... WHERE status='pending_approval'`), выборка
  ожидающих с ленивым авто-reject по таймауту.
- **Handler** (`internal/handler/`): 202 для `pending_approval`; новые роуты
  `/admin/approvals*`; новые доменные ошибки в `service/errors.go` и ветки в
  `handler/errors.go`.
- **API** (`api/openapi.yaml`): описание новых эндпоинтов, статусов и кода 202.
- Новые sentinel-ошибки: `ErrApprovalNotFound`, `ErrNotPendingApproval`,
  `ErrApprovalForbidden`, `ErrApprovalTimedOut` (либо переиспользование существующих
  ролевых/not-found ошибок — уточняется в design).
