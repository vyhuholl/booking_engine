// Command mcp-openapi — минимальный MCP-сервер поверх OpenAPI-спецификации
// booking engine. Ровно два инструмента: list_endpoints и get_endpoint_schema.
// Спецификация читается один раз при старте; ни кэшей, ни watch-еров.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"
)

// httpMethods — ключи path item, которые в OpenAPI являются операциями.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// spec — минимально нужная часть OpenAPI-документа: только paths. Внутри path
// item ключи разнородны (методы → операции, parameters → список), поэтому
// значения держим как any.
type spec struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

// endpoint — одна строка ответа list_endpoints.
type endpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-openapi:", err)
		os.Exit(1)
	}
}

func run() error {
	path, err := specPath()
	if err != nil {
		return err
	}
	sp, err := loadSpec(path)
	if err != nil {
		return err
	}

	s := server.NewMCPServer("openapi", "1.0.0")

	s.AddTool(
		mcp.NewTool("list_endpoints",
			mcp.WithDescription("Список всех эндпоинтов API: массив объектов {method, path, summary}."),
		),
		listEndpoints(sp),
	)
	s.AddTool(
		mcp.NewTool("get_endpoint_schema",
			mcp.WithDescription("Полная схема одного эндпоинта: parameters, requestBody, responses."),
			mcp.WithString("method", mcp.Required(), mcp.Description("HTTP-метод, например GET.")),
			mcp.WithString("path", mcp.Required(), mcp.Description("Шаблон пути как в спецификации, например /rooms/{id}.")),
		),
		getEndpointSchema(sp),
	)

	return server.ServeStdio(s)
}

// specPath — путь к openapi.yaml: первый аргумент командной строки, иначе
// переменная окружения OPENAPI_SPEC_PATH.
func specPath() (string, error) {
	if len(os.Args) > 1 && os.Args[1] != "" {
		return os.Args[1], nil
	}
	if p := os.Getenv("OPENAPI_SPEC_PATH"); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("путь к openapi.yaml не задан: передайте его аргументом или через OPENAPI_SPEC_PATH")
}

func loadSpec(path string) (*spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение спецификации: %w", err)
	}
	var sp spec
	if err := yaml.Unmarshal(raw, &sp); err != nil {
		return nil, fmt.Errorf("разбор OpenAPI: %w", err)
	}
	if len(sp.Paths) == 0 {
		return nil, fmt.Errorf("в спецификации %s нет ни одного пути", path)
	}
	return &sp, nil
}

// listEndpoints перечисляет все операции спецификации, отсортированные по пути.
func listEndpoints(sp *spec) server.ToolHandlerFunc {
	return func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paths := make([]string, 0, len(sp.Paths))
		for p := range sp.Paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		out := make([]endpoint, 0)
		for _, p := range paths {
			item := sp.Paths[p]
			for _, m := range httpMethods {
				op, ok := item[m].(map[string]any)
				if !ok {
					continue
				}
				summary, _ := op["summary"].(string)
				out = append(out, endpoint{Method: strings.ToUpper(m), Path: p, Summary: summary})
			}
		}
		return jsonResult(out)
	}
}

// getEndpointSchema возвращает parameters/requestBody/responses одной операции.
func getEndpointSchema(sp *spec) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		method, err := req.RequireString("method")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		item, ok := sp.Paths[path]
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("путь %q не найден в спецификации", path)), nil
		}
		op, ok := item[strings.ToLower(method)].(map[string]any)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("метод %s не определён для пути %q", strings.ToUpper(method), path)), nil
		}

		schema := map[string]any{
			// parameters уровня path применяются ко всем операциям пути,
			// поэтому сливаем их с параметрами самой операции.
			"parameters":  mergeParameters(item["parameters"], op["parameters"]),
			"requestBody": op["requestBody"],
			"responses":   op["responses"],
		}
		return jsonResult(schema)
	}
}

// mergeParameters объединяет parameters уровня path и уровня операции. По
// OpenAPI параметр операции переопределяет path-level с той же парой (name, in);
// параметры, заданные через $ref, оставляем как есть (name/in неизвестны).
func mergeParameters(pathLevel, opLevel any) []any {
	merged := make([]any, 0)
	seen := map[string]bool{}

	add := func(list any) {
		items, ok := list.([]any)
		if !ok {
			return
		}
		for _, it := range items {
			if p, ok := it.(map[string]any); ok {
				name, _ := p["name"].(string)
				in, _ := p["in"].(string)
				if name != "" && in != "" {
					key := in + "\x00" + name
					if seen[key] {
						continue
					}
					seen[key] = true
				}
			}
			merged = append(merged, it)
		}
	}

	// Операционные параметры добавляем первыми, чтобы они «побеждали»
	// одноимённые path-level.
	add(opLevel)
	add(pathLevel)
	return merged
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("сериализация ответа: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}
