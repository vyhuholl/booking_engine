# Design: Rooms List Endpoint

## Context

Текущий эндпоинт `GET /rooms` уже существует в кодобазе и поддерживает:
- Пагинацию (limit/offset)
- Фильтрацию по этажу (`floor`)

Требуется расширить функциональность согласно спецификации:
- Добавить фильтрацию по минимальной вместимости (`min_capacity`)
- Возвращать только активные комнаты (`status = 'active'`)
- Изменить сортировку с `floor, name` на `name` только

Существующая архитектура: handler → service → repository с слоистой разделённостью. Все слои уже реализованы для rooms, изменения локальны.

## Goals / Non-Goals

**Goals:**
- Добавить query-параметр `min_capacity` для фильтрации по вместимости
- Гарантировать возврат только активных комнат (hardcoded filter)
- Сортировка результатов по имени (алфавитно)
- Валидация типов query-параметров с возвратом HTTP 400
- Сохранить обратную совместимость с существующей пагинацией

**Non-Goals:**
- Удаление существующей пагинации (limit/offset) — остаётся для backward compatibility
- Изменение структуры ответа (остаётся `{items: [], total: int}`)
- Добавление кэширования (out of scope для данного изменения)

## Decisions

### 1. Расширение RoomFilter в repository

**Решение:** Добавить поле `MinCapacity *int` в `repository.RoomFilter`.

**Альтернативы:**
- Создать отдельный метод `ListActive` — отклонено из-за дублирования кода
- Передавать min_capacity как отдельный аргумент в `List` — отклонено, ломает интерфейс

**Обоснование:** `RoomFilter` уже используется для floor, расширение для capacity следует существующему паттерну.

### 2. Hardcoded фильтрация по статусу

**Решение:** Добавить `WHERE status = 'active'` непосредственно в SQL-запрос `repository.Room.List`.

**Обоснование:** Согласно спецификации, эндпоинт возвращает только активные комнаты. Это бизнес-правило уровня репозитория для данного конкретного query. Альтернатива — добавить `Status` в `RoomFilter`, но:
- Нет требований для фильтрации по другим статусам
- Усложняет API без пользы
- В будущем при необходимости можно расширить

### 3. Сортировка по name вместо floor, name

**Решение:** Изменить `ORDER BY floor, name` на `ORDER BY name` в SQL-запросе.

**Риск:** Возможное ухудшение UX для пользователей, привыкших к группировке по этажам.

**Митигация:** Клиентские приложения могут сортировать результаты на своей стороне при необходимости. Если в будущем появится потребность в динамической сортировке, можно добавить параметр `sort_by`.

### 4. Обработка валидации в handler

**Решение:** Валидация query-параметров (`floor`, `min_capacity`) остаётся в handler (существующий паттерн).

**Обоснование:** Handler уже отвечает за парсинг и валидацию query-параметров (см. строку 47-54 в `handler/room.go`). Service не должен знать про HTTP-детали.

### 5. Ответ без пагинации

**Решение:** Пагинация остаётся для backward compatibility, но при отсутствии `limit` в запросе возвращаются все результаты.

**Обоснование:** Существующий код использует пагинацию. Удаление сломает текущих клиентов. Новые клиенты могут просто не передавать `limit/offset`.

## Implementation Approach

### Changes by layer

**Repository (`internal/repository/room.go`):**
```go
type RoomFilter struct {
    Floor       *int
    MinCapacity *int  // NEW
    Limit       int
    Offset      int
}

func (r *Room) List(ctx context.Context, f RoomFilter) ([]model.Room, int, error) {
    // Добавить WHERE для:
    // - status = 'active' (hardcoded)
    // - capacity >= $n (если MinCapacity задан)
    // Изменить ORDER BY на name ASC
}
```

**Service (`internal/service/room.go`):**
```go
// Метод List уже существует, изменения не требуются
// RoomFilter передаётся прозрачно в repository
```

**Handler (`internal/handler/room.go`):**
```go
func (h *Handler) listRooms(w http.ResponseWriter, r *http.Request) {
    f := repository.RoomFilter{
        Limit:  parseInt(r.URL.Query().Get("limit"), 50),
        Offset: parseInt(r.URL.Query().Get("offset"), 0),
    }
    // Существующий код для floor
    // NEW: добавить парсинг min_capacity с валидацией
    if v := r.URL.Query().Get("min_capacity"); v != "" {
        n, err := strconv.Atoi(v)
        if err != nil {
            writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "min_capacity must be an integer", nil)
            return
        }
        f.MinCapacity = &n
    }
    // ... rest of handler
}
```

### SQL Query Changes

**Текущий запрос:**
```sql
SELECT id, name, capacity, floor, equipment, status FROM rooms
[WHERE floor = $1]
ORDER BY floor, name LIMIT $n OFFSET $m
```

**Новый запрос:**
```sql
SELECT id, name, capacity, floor, equipment, status FROM rooms
WHERE status = 'active'
  [AND floor = $1]
  [AND capacity >= $n]
ORDER BY name ASC
LIMIT $x OFFSET $y
```

**Порядок параметров:** При комбинированной фильтрации порядок аргументов важен:
1. Если задан только `floor` → `$1 = floor`
2. Если задан только `min_capacity` → `$1 = min_capacity`
3. Если оба → `$1 = floor`, `$2 = min_capacity`

### Parameter indexing strategy

Для корректной нумерации placeholders в динамическом SQL:
- Создавать слайс условий WHERE
- Добавлять параметры в args по мере формирования условий
- Использовать `len(args) + 1` для нумерации placeholders

## Risks / Trade-offs

### Risk: Обратная совместимость
[Risk] Изменение сортировки с `floor, name` на `name` может сломать UI клиентов, зависящих от группировки по этажам.

**Mitigation:** Данное изменение отражено в спецификации как требование. Клиенты должны быть уведомлены об изменении порядка сортировки через документацию/API changelog.

### Risk: SQL injection через динамический запрос
[Risk] Динамическое построение WHERE-клаузы может привести к ошибкам в нумерации placeholders.

**Mitigation:** Использовать существующий паттерн с массивом `args` и функцией `itoa` для конвертации индекса. Покрыть тестами все комбинации фильтров.

### Risk: Отрицательные значения в фильтрах
[Risk] `floor=-1` или `min_capacity=-5` валидны синтаксически, но семантически бессмысленны.

**Mitigation:** Согласно спеку, отрицательные значения допустимы и просто вернут пустой результат. Нет требования для дополнительной валидации диапазона.

## Open Questions

Нет. Все решения приняты, спецификация полная.

## Testing Strategy

1. **Unit tests (service):** не требуются — бизнес-логика не меняется
2. **Integration tests (repository):** добавить тесты для `List` с фильтрами:
   - Только floor
   - Только min_capacity
   - Оба фильтра
   - Без фильтров
   - Проверка сортировки
   - Проверка фильтрации по статусу
3. **HTTP tests (handler):** добавить тесты для query-параметров и валидации

## Migration Plan

Изменение обратно совместимо (добавляются optional query-параметры). Деплой без специального миграционного плана.

**Rollback strategy:** Если возникнут проблемы с сортировкой, можно быстро откатить изменение `ORDER BY` на `floor, name` без удаления фильтров.
