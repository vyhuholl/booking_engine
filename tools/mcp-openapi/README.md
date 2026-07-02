# mcp-openapi

Минимальный MCP-сервер (stdio) поверх `api/openapi.yaml`. Отдельный Go-модуль,
чтобы зависимость `mcp-go` не попадала в go.mod основного сервиса.

## Запуск

Путь к спецификации — первым аргументом или через `OPENAPI_SPEC_PATH`:

```sh
go run . ../../api/openapi.yaml
# или
OPENAPI_SPEC_PATH=../../api/openapi.yaml go run .
```

Спецификация читается один раз при старте.

## Инструменты

- **`list_endpoints`** — без параметров. Возвращает JSON-массив
  `[{ "method", "path", "summary" }, ...]` по всем операциям, отсортированный по пути.
- **`get_endpoint_schema`** — параметры `method` (string), `path` (string).
  Возвращает `{ "parameters", "requestBody", "responses" }` операции
  (`parameters` уровня path слиты с параметрами операции). Если путь или метод
  не найдены — ошибка инструмента.

## Регистрация в `.mcp.json`

```json
{
  "mcpServers": {
    "openapi": {
      "type": "stdio",
      "command": "go",
      "args": ["run", ".", "../../api/openapi.yaml"],
      "env": {}
    }
  }
}
```
