# Тестовые сценарии: BookingService

Источник истины для table-driven тестов в [`internal/service/booking_service_test.go`](../internal/service/booking_service_test.go).

## Бизнес-правила

1. Нельзя забронировать комнату, если она занята в запрошенный интервал.
2. Бронирование минимум 15 минут, максимум 8 часов.
3. Отмена не позднее чем за 30 минут до начала.
4. Manager может отменять чужие бронирования на своём этаже.
5. Admin может всё.

## BookingService.CreateBooking

| ID | Категория | Описание | Входные данные | Ожидаемый результат |
|---|---|---|---|---|
| TC-001 | happy path | Member бронирует свободную комнату на 1 час | user=member, room=R1 (свободна), 10:00–11:00 | 201 Created, бронь сохранена, возвращён booking_id |
| TC-002 | happy path | Manager бронирует комнату на своём этаже | user=manager(floor=2), room=R2(floor=2), 14:00–15:30 | 201 Created, бронь сохранена |
| TC-003 | happy path | Admin бронирует любую комнату | user=admin, room=R5, 09:00–10:00 | 201 Created, бронь сохранена |
| TC-004 | edge case | Бронь ровно 15 минут (нижняя граница) | 10:00–10:15 | 201 Created |
| TC-005 | validation | Бронь 14 минут (ниже минимума) | 10:00–10:14 | 400 Bad Request, "minimum booking duration is 15 minutes" |
| TC-006 | edge case | Бронь ровно 8 часов (верхняя граница) | 09:00–17:00 | 201 Created |
| TC-007 | validation | Бронь 8 часов 1 минута (выше максимума) | 09:00–17:01 | 400 Bad Request, "maximum booking duration is 8 hours" |
| TC-008 | validation | Admin тоже ограничен длительностью — 14 минут | user=admin, 10:00–10:14 | 400 Bad Request, "minimum booking duration is 15 minutes" |
| TC-009 | conflict | Полное пересечение с существующей бронью | существует 10:00–12:00; запрос 10:00–12:00 | 409 Conflict, "room is already booked" |
| TC-010 | conflict | Частичное пересечение (начало в чужом окне) | существует 10:00–12:00; запрос 11:00–13:00 | 409 Conflict |
| TC-011 | conflict | Частичное пересечение (конец в чужом окне) | существует 10:00–12:00; запрос 09:00–11:00 | 409 Conflict |
| TC-012 | conflict | Запрос полностью внутри существующей брони | существует 10:00–12:00; запрос 10:30–11:30 | 409 Conflict |
| TC-013 | conflict | Запрос полностью охватывает существующую бронь | существует 10:30–11:30; запрос 10:00–12:00 | 409 Conflict |
| TC-014 | edge case | Граничное касание: новая начинается ровно в конце старой | существует 10:00–11:00; запрос 11:00–12:00 | 201 Created (конец-в-начало не считается конфликтом) |
| TC-015 | edge case | Граничное касание: новая заканчивается ровно в начале старой | существует 11:00–12:00; запрос 10:00–11:00 | 201 Created |
| TC-016 | validation | Пустое поле room_id | room_id=null или "", 10:00–11:00 | 400 Bad Request, "room_id is required" |
| TC-017 | validation | Пустое поле start_time | start_time=null, end_time=11:00 | 400 Bad Request, "start_time is required" |
| TC-018 | validation | Пустое поле end_time | start_time=10:00, end_time=null | 400 Bad Request, "end_time is required" |
| TC-019 | validation | Пустое поле title | title="" или whitespace | 400 Bad Request, "title is required" |
| TC-020 | validation | Прошедшее время начала (с учётом TZ пользователя) | start_time=вчера 10:00 в TZ юзера | 400 Bad Request, "start_time must be in the future" |
| TC-021 | validation | end_time == start_time | 10:00–10:00 | 400 Bad Request, "end_time must be after start_time" |
| TC-022 | validation | end_time < start_time | 11:00–10:00 | 400 Bad Request, "end_time must be after start_time" |
| TC-023 | validation | Несуществующая комната | room_id="ghost-123" | 404 Not Found, "room not found" |
| TC-024 | validation | Удалённая (soft-deleted) комната | room.deleted_at != null | 404 Not Found, "room not found" |
| TC-025 | validation | Комната выведена из эксплуатации | room.status="out_of_service" | 409 Conflict, "room is not available for booking" |
| TC-026 | authorization | Неаутентифицированный запрос | без токена | 401 Unauthorized (проверяется на handler-слое) |
| TC-027 | edge case | Параллельный запрос на тот же слот (race condition) | два одновременных POST на 10:00–11:00 | один — 201, второй — 409 Conflict |

## BookingService.CancelBooking

| ID | Категория | Описание | Входные данные | Ожидаемый результат |
|---|---|---|---|---|
| TC-028 | happy path | Member отменяет свою бронь за 2 часа до начала | owner=member; now=08:00; start=10:00 | 204 No Content, бронь помечена cancelled |
| TC-029 | edge case | Отмена ровно за 30 минут до начала (граница) | now=09:30; start=10:00 | 204 No Content |
| TC-030 | validation | Отмена за 29 минут до начала | now=09:31; start=10:00 | 400 Bad Request, "cancellation allowed no later than 30 minutes before start" |
| TC-031 | validation | Отмена после начала брони | now=10:15; start=10:00 | 400 Bad Request, "cannot cancel started/past booking" |
| TC-032 | edge case | Граница «30 минут» с учётом часового пояса | start=10:00 Europe/Moscow; пользователь в Asia/Tokyo, его локальные 16:30 == МСК 09:30 | 204 No Content (расчёт ведётся в общем UTC, а не в локальной строке) |
| TC-033 | authorization | Member пытается отменить чужую бронь | user=member1, booking.owner=member2 | 403 Forbidden, "cannot cancel another user's booking" |
| TC-034 | happy path | Manager отменяет чужую бронь на своём этаже | user=manager(floor=2), booking.room.floor=2, owner=member | 204 No Content |
| TC-035 | authorization | Manager пытается отменить чужую бронь на чужом этаже | user=manager(floor=2), booking.room.floor=3 | 403 Forbidden, "manager can cancel only on own floor" |
| TC-036 | happy path | Manager отменяет свою бронь на чужом этаже | user=manager(floor=2), owner=manager, booking.room.floor=3 | 204 No Content (как владелец) |
| TC-037 | happy path | Manager отменяет бронь admin'а на своём этаже | user=manager(floor=2), owner=admin, room.floor=2 | 204 No Content (правило #4 не делает исключения по роли владельца) |
| TC-038 | happy path | Admin отменяет любую бронь на любом этаже | user=admin, owner=member, floor=любой | 204 No Content |
| TC-039 | validation | Admin тоже ограничен правилом «за 30 минут» | user=admin, now=09:50, start=10:00 | 400 Bad Request, "cancellation allowed no later than 30 minutes before start" |
| TC-040 | validation | Отмена несуществующей брони | booking_id="ghost-999" | 404 Not Found |
| TC-041 | validation | Повторная отмена уже отменённой брони | booking.status=cancelled | 409 Conflict, "booking already cancelled" |
| TC-042 | validation | Пустой booking_id | booking_id="" | 400 Bad Request, "booking_id is required" |
| TC-043 | authorization | Неаутентифицированный запрос на отмену | без токена | 401 Unauthorized (проверяется на handler-слое) |

## Ключевые решения, зафиксированные с PO

- Admin подчиняется правилам #2 (длительность) и #3 (за 30 минут до начала). Правило #5 трактуется как «любая комната, любой пользователь», но не как обход технических ограничений.
- Manager может отменять бронь admin'а на своём этаже (правило #4 — по этажу, не по роли владельца).
- Все темпоральные проверки (`в будущем`, `за 30 минут`) ведутся в общем UTC; входящие значения нормализуются из TZ клиента.
- Формат `room_id` не валидируется на сервисе: любой нераспознанный ID превращается в 404 Not Found (поглощается TC-023). Отдельной 400-ошибки для «битого формата» нет — это сокращает дублирующую логику.
- Мультитенантность вне скоупа сервиса — модели тенанта/организации в системе нет.

## Покрытие тестами

Все сценарии из таблицы реализованы в [`booking_service_test.go`](../internal/service/booking_service_test.go), кроме одного:

| ID | Где покрывается / статус |
|---|---|
| TC-026 (auth на Create) | Handler-слой: [`handler.authMiddleware`](../internal/handler/auth.go). В service-тестах помечен `t.Skip` с пояснением. |
| TC-043 (auth на Cancel) | Аналогично — handler middleware. |
