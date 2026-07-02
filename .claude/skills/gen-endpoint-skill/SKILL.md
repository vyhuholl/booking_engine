---
name: gen-endpoint-skill
description: Сгенерировать новый REST-эндпоинт для booking engine — хендлер, метод сервиса, метод репозитория, unit-тесты (table-driven на testutil), при необходимости миграцию, и зарегистрировать маршрут. Использовать, когда просят добавить/сгенерировать эндпоинт, ручку, роут или CRUD-операцию в booking engine.
---

# Генерация эндпоинта в booking engine

Ты добавляешь новый эндпоинт в слоистое Go-приложение (`handler → service → repository`).
Аргумент к скиллу — **описание эндпоинта**: HTTP-метод, путь и что он делает
(например: `POST /rooms/{id}/lock — админ временно блокирует комнату от бронирований`).

Действуй в режиме `implement` из корневого CLAUDE.md: пиши код, следуй конвенциям,
**новый код обязан иметь тесты**. Не коммить без запроса пользователя.

## 0. Разбор задачи (сделай до кода)

1. **Ресурс** — существующая сущность (`booking`, `room`, `user`, `waitlist`) или новая?
   Файл в слое называется по сущности: `booking.go`, `room.go`, `waitlist.go`.
   - Сущность уже есть → **добавляй метод в существующие** `internal/{layer}/{resource}.go`
     и соответствующие `*_test.go`. Не плоди новые файлы.
   - Сущность новая → создай `internal/handler/{resource}.go`,
     `internal/service/{resource}.go`, `internal/repository/{resource}.go` и тесты рядом.

   > ⚠️ Именование: в этом проекте **нет** суффиксов `_handler`/`_service`/`_repository`
   > (правило CLAUDE.md «один файл на сущность в слое»). Используй `{resource}.go`,
   > а не `{resource}_handler.go`. Все существующие файлы следуют этому — не отклоняйся.

2. **Нужна ли схема БД?** Новая таблица/колонка/индекс → новая **пара** миграций
   `migrations/NNN_name.up.sql` + `.down.sql` (см. §5). Меняешь только через миграции.

3. **Вход**: path-param `{id}` / query-string / JSON-body — определяет парсинг в хендлере.

4. **Авторизация**: кто может звать (любой аутентифицированный / `IsAdmin` / владелец-или-admin
   / менеджер этажа)? Это правило живёт **в сервисе**.

5. **Побочные эффекты**: меняет занятость комнаты? Тогда после успеха — инвалидация кэша
   доступности + публикация события в Kafka (как в `Booking.Create` / `Waitlist.Confirm`).

6. **Новые доменные ошибки?** → sentinel в `service/errors.go` **и** ветка в `handler/errors.go`.

Поток строго вниз: handler ничего не решает, repository ничего не решает — вся логика в service.

## 1. Repository (`internal/repository/{resource}.go`)

Только SQL через `pgx`. Никакой бизнес-логики. Правила:

- Сигнатура: `(ctx context.Context, <конкретные параметры>) (model.X, error)` — параметры,
  **не** структуры запроса (исключение — уже существующие `RoomFilter`/`UserBookingFilter`).
- Все запросы параметризованы (`$1, $2`), никогда `fmt.Sprintf` со значениями.
- Возвращай доменные `model.*`.
- Трансляция ошибок: `wrapNoRows(err)` → `ErrNotFound`; `isUniqueViolation(err)` → `ErrConflict`.
- `timestamptz` для всех временных колонок; в Go всегда пиши/читай `.UTC()`.
- Многошаговую атомарную операцию (проверка+запись, «сделать и предложить следующему»)
  оборачивай в `runSerializable` — она сама ретраит сериализационные сбои. `fn` **не**
  коммитит/откатывает и обязана сбрасывать возвращаемые-через-замыкание значения в начале.

Простой метод (мутация с проверкой существования):

```go
func (r *Room) Lock(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE rooms SET status = 'out_of_service' WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
```

Чтение одной строки:

```go
func (r *Room) GetSomething(ctx context.Context, id string) (model.Room, error) {
	var room model.Room
	err := r.pool.QueryRow(ctx,
		`SELECT id, name, capacity, floor, equipment, status FROM rooms WHERE id = $1`, id,
	).Scan(&room.ID, &room.Name, &room.Capacity, &room.Floor, &room.Equipment, &room.Status)
	if err != nil {
		return model.Room{}, wrapNoRows(err)
	}
	return room, nil
}
```

Атомарная операция (шаблон):

```go
func (r *Room) DoAtomic(ctx context.Context, id string) (result *model.X, err error) {
	err = runSerializable(ctx, r.pool, func(tx pgx.Tx) error {
		result = nil // сброс перед каждой попыткой
		// ... tx.QueryRow / tx.Exec ...
		// нарушение уникальности:
		//   if isUniqueViolation(execErr) { return ErrConflict }
		// штатный откат без ретрая (напр. обнаружен конфликт):
		//   return errRollback
		return nil
	})
	if errors.Is(err, errRollback) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}
```

**Интеграционный тест** — пакет `repository_test`, реальный Postgres (нужен Docker), в `{resource}_repository_test.go`:

```go
func TestRoom_Lock(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewRoom(pool)
	ctx := context.Background()

	t.Run("locks existing room", func(t *testing.T) {
		cleanup() // изоляция подтеста: TRUNCATE всех таблиц
		roomID := testutil.SeedRoom(t, pool)

		require.NoError(t, repo.Lock(ctx, roomID))

		got, err := repo.Get(ctx, roomID)
		require.NoError(t, err)
		assert.Equal(t, model.RoomStatusOutOfService, got.Status)
	})

	t.Run("missing room returns ErrNotFound", func(t *testing.T) {
		cleanup()
		assert.ErrorIs(t, repo.Lock(ctx, "room-ghost"), repository.ErrNotFound)
	})
}
```

Сеять данные только хелперами: `testutil.SeedRoom`, `SeedUser`, `SeedBooking`,
`SeedWaitlist`, `FreshRoomAndUser(t, pool, cleanup)`. `cleanup()` вызывать **в начале
каждого подтеста**. Пустой список из репозитория проверяй как непустой срез (не nil).

## 2. Service (`internal/service/{resource}.go`)

Вся бизнес-логика: валидация, авторизация, генерация ID, нормализация времени, оркестрация.

- Интерфейс репозитория объявляется **здесь, в потребителе** (например `RoomRepo`, `BookingRepo`,
  `WaitlistRepo`), а не в пакете repository. Расширяешь возможности — добавь метод в этот интерфейс.
- Метод: `(ctx context.Context, a Actor, <input>) (model.X, error)`. `Actor` — **явный** аргумент,
  даже если не используется (`_ Actor`). Авторизация только здесь.
- Входной DTO — `XxxCreateInput` / `XxxUpdateInput` (экспортируемый), не тело HTTP-запроса.
- Время — только через поле `now func() time.Time` (`s.now()`), **никогда** `time.Now()` напрямую;
  всё время в БД/событиях приводи к `.UTC()`.
- ID новой сущности: префикс + uuid — `"b-"+uuid.NewString()`, `"r-"`, `"wl-"`, `"user-"`.
- Валидация → `&ValidationError{Field, Message}`; общий интервал брони/waitlist → `validateInterval`.
- Транслируй repo-ошибки в доменные: `repository.ErrNotFound` → `ErrRoomNotFound`/`ErrBookingNotFound`;
  `repository.ErrConflict` → доменный sentinel. Не пропускай сырые SQL-ошибки наверх.
- Логирование: `log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "room_id", ...)`
  в начале; `log.Info(...)` на успешное изменение состояния; `log.Error("...", slog.Any("error", err))`
  на сбой. Никаких `fmt.Println`/`log.Printf`, никаких секретов/PII.

```go
// Lock выводит комнату из обслуживания. Только admin.
func (s *Room) Lock(ctx context.Context, a Actor, id string) (model.Room, error) {
	log := s.log.With("trace_id", tracing.TraceID(ctx), "user_id", a.ID, "room_id", id)

	if !a.IsAdmin() {
		return model.Room{}, ErrForbidden
	}
	room, err := s.getOr404(ctx, id) // getOr404 уже мапит ErrNotFound → ErrRoomNotFound
	if err != nil {
		return model.Room{}, err
	}
	if room.Status == model.RoomStatusOutOfService {
		return model.Room{}, ErrRoomOutOfService
	}
	if err := s.rooms.Lock(ctx, id); err != nil {
		log.Error("lock room", slog.Any("error", err))
		return model.Room{}, mapRoomNotFound(err)
	}
	room.Status = model.RoomStatusOutOfService

	// Если операция влияет на занятость — сбросить кэш и опубликовать событие:
	// invalidateRoomCache(ctx, s.cache, log, id)
	// publishBookingEvent(ctx, s.publisher, s.topic, log, events.TypeXxx, b, s.now())

	log.Info("room locked")
	return room, nil
}
```

**Unit-тест** — пакет `service`, без БД, table-driven, в `{resource}_service_test.go`.
Моки — ручные структуры с полями-функциями (`getFn`, `createFn`, …); незаданный метод
**паникует** (это ловит неожиданные вызовы). Используй уже существующие в пакете
`mockRoomLookup`/`mockBookingRepo`/`mockWaitlistRepo` и хелперы `newTestService`,
`roomFound`, `testActor`, `fixedNow`/`baseStart`/`baseEnd`, `testRoomID`. Фикстуры — из
`testutil` (см. §4). Часы подменяй `s.now = func() time.Time { return fixedNow }` (или
`testutil.Clock(testutil.FixedNow)`).

```go
func TestRoomService_Lock(t *testing.T) {
	cases := []struct {
		name      string
		actor     Actor
		setup     func(rooms *mockRoomLookup)
		wantErrIs error
	}{
		{
			name:  "admin locks active room",
			actor: Actor{ID: testUserID, Role: model.RoleAdmin},
			setup: func(rooms *mockRoomLookup) { roomFound(rooms, 2) },
		},
		{
			name:      "member forbidden",
			actor:     testActor(model.RoleMember),
			setup:     func(rooms *mockRoomLookup) {}, // проверка роли до похода в репозиторий
			wantErrIs: ErrForbidden,
		},
		{
			name:  "room not found",
			actor: Actor{ID: testUserID, Role: model.RoleAdmin},
			setup: func(rooms *mockRoomLookup) {
				rooms.getFn = func(context.Context, string) (model.Room, error) {
					return model.Room{}, repository.ErrNotFound
				}
			},
			wantErrIs: ErrRoomNotFound,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rooms := &mockRoomLookup{}
			tc.setup(rooms)
			svc := newTestRoomService(rooms /*, ...*/) // соберёт *Room с s.now = fixedNow

			got, err := svc.Lock(context.Background(), tc.actor, testRoomID)
			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, model.RoomStatusOutOfService, got.Status)
		})
	}
}
```

Покрой ветки: успех, каждая доменная ошибка, отказ авторизации, конфликт/not-found от репо.
Для `ValidationError` — `testutil.AssertServiceError(t, err, nil, new(*ValidationError))`
или `assert.ErrorAs`. Для sentinel — `assert.ErrorIs` / `testutil.AssertSentinel`.

## 3. Handler (`internal/handler/{resource}.go`)

Только разбор запроса, вызов сервиса, форматирование ответа. **Ноль** бизнес-логики.
Хендлер — unexported `verbNoun` (`createBooking`, `listRooms`, `lockRoom`).

Скелет:

```go
type roomLockBody struct { // тело запроса — unexported, только если есть JSON-body
	Reason string `json:"reason"`
}

// lockRoom — POST /rooms/{id}/lock.
func (h *Handler) lockRoom(w http.ResponseWriter, r *http.Request) {
	var body roomLockBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body", nil)
		return
	}
	room, err := h.rooms.Lock(r.Context(), actorFromCtx(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err) // маппинг доменной ошибки → HTTP-код, ВСЕГДА через него
		return
	}
	writeJSON(w, http.StatusOK, room)
}
```

Правила:
- Достать вход: `chi.URLParam(r, "id")`, `r.URL.Query().Get(...)` (числа — через `parseInt`,
  время — `parseRFC3339` / `time.Parse("2006-01-02", ...)`), или `json.NewDecoder(r.Body).Decode`.
- Ошибка декода/парса query → сразу `writeError(w, 400, "VALIDATION_ERROR", ...)`.
- Актор: `actorFromCtx(r.Context())`. Контекст запроса всегда прокидывай `r.Context()`.
- Любую ошибку сервиса отдавай **только** через `writeServiceError(w, err)` — не хардкодь коды.
- Коды успеха: `200` чтение/обновление, `201` создание (`writeJSON(w, http.StatusCreated, ...)`),
  `204` удаление (`w.WriteHeader(http.StatusNoContent)` или `writeJSON(w, 204, nil)`).
- Ответ — либо `model.*` напрямую, либо обёртка-DTO (`roomListResponse{Items, Total}`),
  если нужны доп. поля/пагинация.

**Handler-тест** (`{resource}_test.go`, пакет `handler`) — по желанию, но следуй существующему
стилю: стаб сервиса (встраивание интерфейса + нужные методы), `httptest`, хелпер запроса,
кладущий `{id}` и актора в контекст в обход роутера (см. `wlRequest` в `waitlist_test.go`).
Проверяй HTTP-код и `code` в теле (`errorCode(t, rec)`).

## 4. Хелперы testutil (обязательно в тестах)

Не собирай `model.*{...}` вручную. Дефолты детерминированы (`FixedNow`, `BaseStart`,
`BaseEnd`, `RoomID`, `UserID`, `AdminID`, `BookingID`).

- **Object mothers** (свежие уникальные id, без опций) — «просто объект»:
  `testutil.TestRoom(t)`, `TestLargeRoom(t)`, `TestUser(t, "member")`, `TestAdmin(t)`,
  `TestManager(t)`, `TestBooking(t)`, `TestConflictingBooking(t, existing)`.
- **Фикстуры с опциями** (детерминированные id) — конкретный набор полей:
  `testutil.Room(WithFloor(3), WithCapacity(10), WithRoomStatus(...), WithEquipment(...))`,
  `User(WithRole(...), WithManagesFloor(3), WithEmail(...))`,
  `Booking(WithRoom(id), WithOwner(id), WithInterval(s,e), WithDuration(d), WithBookingStatus(...))`.
- **Билдер брони** (цепочечный, свежий uuid, дефолтные комната/member):
  `testutil.NewBookingBuilder(t).WithRoom(testutil.TestRoom(t)).WithUser(u).WithTime(s,e).WithStatus("confirmed").Build()`.
- **Часы**: `testutil.Clock(testutil.FixedNow)` для подмены поля `now`.
- **Ассерты ошибок**: `AssertServiceError(t, err, wantIs, wantAs)`, `AssertSentinel(t, err, want)`,
  `AssertTyped[E](t, err)`.
- **Интеграционные (Docker)**: `SetupTestDB(t)` → `(pool, cleanup)`; сиды
  `SeedRoom`/`SeedUser`/`SeedBooking`/`SeedWaitlist` (принимают те же опции фикстур);
  `FreshRoomAndUser(t, pool, cleanup)`.

## 5. Миграция (только если меняется схема)

Новая **пара** `migrations/NNN_name.up.sql` и `migrations/NNN_name.down.sql`, `NNN` —
следующий по возрастанию (текущий максимум — `003`; посмотри `ls migrations/`). Оборачивай в
`BEGIN; … COMMIT;`. `TEXT PRIMARY KEY` для id, `TIMESTAMPTZ` для времени, enum'ы через
`CREATE TYPE ... AS ENUM`. `.down.sql` откатывает ровно то, что создал `.up.sql`
(`DROP ... IF EXISTS` в обратном порядке).

`004_add_room_lock.up.sql`:

```sql
BEGIN;

ALTER TABLE rooms ADD COLUMN locked_reason TEXT;

COMMIT;
```

`004_add_room_lock.down.sql`:

```sql
BEGIN;

ALTER TABLE rooms DROP COLUMN IF EXISTS locked_reason;

COMMIT;
```

После миграции обнови комментарий-схему в `internal/model/model.go`, если он есть у сущности.

## 6. Регистрация маршрута

В `internal/handler/handler.go`, функция `Router()`, **внутри группы с `h.authMiddleware`**
(эндпоинты без auth — только `/healthz`). Добавь строку в нужный `r.Route(...)`:

```go
r.Route("/rooms", func(r chi.Router) {
	// ... существующие ...
	r.Post("/{id}/lock", h.lockRoom)
})
```

Если создаёшь **новый сервис** — проведи его через wiring:
1. Поле в `struct Handler` и параметр в `func New(...)` (`internal/handler/handler.go`).
2. Конструктор `repository.NewXxx(pool)` и `service.NewXxx(...)` в `cmd/server/main.go`
   (блок wiring), и передай сервис в `handler.New(...)`.

## 7. Финальная проверка

- [ ] Маршрут в `Router()` под `authMiddleware`.
- [ ] Новая доменная ошибка → sentinel в `service/errors.go` **и** ветка в `handler/errors.go`
      (HTTP-код `SCREAMING_SNAKE`).
- [ ] Новый сервис проведён через `handler.New` и `cmd/server/main.go`.
- [ ] Тесты: `go test ./internal/service/...` (без Docker) и, при наличии Docker,
      `go test ./internal/repository/... ./internal/handler/...`.
- [ ] `go build ./...` проходит.
- [ ] Обновлён `api/openapi.yaml` (новый путь, тело, коды ответов).

## Инварианты (не нарушать)

- Бизнес-логика — только в service. Handler не валидирует/не авторизует. Repository не решает.
- Интерфейсы — в потребителе (service/handler), не в реализующем пакете.
- Только параметризованный SQL. `context.Context` — первый аргумент каждого публичного метода.
- Время — через `s.now()`; всё, что идёт в БД/Kafka — `.UTC()`.
- Только `log/slog`, инжектированный через конструктор. Никаких секретов/PII в логах.
- Новый код без тестов не считается готовым.
