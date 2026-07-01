Система бронирования переговорных комнат в коворкинге: REST API на Go, PostgreSQL, JWT-аутентификация. Комнаты, брони с проверкой пересечений, ролевой доступ (admin/manager/member).

## Стек

- **Go 1.25**, роутер `go-chi/chi/v5`, логи `log/slog` (JSON).
- **PostgreSQL** через `jackc/pgx/v5` (`pgxpool`), сырой SQL (без ORM).
- **JWT** — `golang-jwt/jwt/v5` (HS256, `sub` = user id).
- **Тесты** — `stretchr/testify`, интеграционные на `testcontainers-go` + модуль `postgres`.

## Структура

- `cmd/server/` — точка входа: конфиг из env, wiring слоёв, graceful shutdown.
- `cmd/devtoken/` — генератор dev-JWT: `JWT_SECRET=... go run ./cmd/devtoken <user_id>`.
- `internal/handler/` — HTTP: роутинг, JSON, auth-middleware, маппинг ошибок.
- `internal/service/` — бизнес-логика, валидация, авторизация.
- `internal/repository/` — доступ к БД (SQL).
- `internal/model/` — доменные типы и enum'ы (общие для всех слоёв).
- `internal/testutil/` — общий setup БД для интеграционных тестов.
- `migrations/` — SQL-миграции `NNN_name.up.sql` / `.down.sql` (применяются по возрастанию).
- `api/openapi.yaml` — спецификация API; `docs/test-scenarios.md` — сценарии.