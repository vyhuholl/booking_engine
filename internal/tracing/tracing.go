// Package tracing переносит W3C trace-контекст (traceparent) через context.Context
// по всей цепочке запроса handler → service → repository → cache → kafka и достаёт
// из него trace_id для логов. Экспортёра трейсов здесь нет (ни Jaeger, ни OTLP):
// пакет только сшивает логи одного запроса по общему trace_id.
//
// Формат traceparent — W3C Trace Context:
//
//	version "-" trace-id "-" span-id "-" flags
//	00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
//
// (version — 2 hex, trace-id — 32 hex, span-id — 16 hex, flags — 2 hex.)
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// HeaderName — имя как HTTP-заголовка, так и Kafka-заголовка с trace-контекстом.
// Одно имя на обеих границах: HTTP-вход кладёт traceparent в контекст, продюсер
// перекладывает его в заголовок сообщения, consumer читает его оттуда же.
const HeaderName = "traceparent"

type ctxKey int

const traceparentKey ctxKey = iota

// WithTraceparent кладёт traceparent в контекст. Пустая строка игнорируется,
// чтобы не затирать уже лежащий в контексте контекст пустым значением.
func WithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return context.WithValue(ctx, traceparentKey, traceparent)
}

// Traceparent возвращает traceparent из контекста ("" — если его там нет).
func Traceparent(ctx context.Context) string {
	tp, _ := ctx.Value(traceparentKey).(string)
	return tp
}

// TraceID возвращает trace-id (второе поле traceparent) из контекста или "",
// если traceparent отсутствует либо некорректен. Именно это значение попадает
// в поле лога trace_id.
func TraceID(ctx context.Context) string {
	parts := strings.Split(Traceparent(ctx), "-")
	if len(parts) != 4 {
		return ""
	}
	return parts[1]
}

// EnsureTraceparent возвращает incoming, если это валидный W3C traceparent,
// иначе генерирует новый. Вызывается на входе HTTP-хендлера: клиентский заголовок
// используется как есть, а его отсутствие/мусор заменяется свежим контекстом.
func EnsureTraceparent(incoming string) string {
	if isValidTraceparent(incoming) {
		return incoming
	}
	return NewTraceparent()
}

// NewTraceparent генерирует новый traceparent со случайными trace-id и span-id
// и выставленным флагом sampled (01).
func NewTraceparent() string {
	traceID := make([]byte, 16)
	spanID := make([]byte, 8)
	// crypto/rand.Read из буфера в памяти не возвращает ошибку; на всякий случай
	// её игнорируем — trace-id остаётся нулевым лишь при системном сбое rand.
	_, _ = rand.Read(traceID)
	_, _ = rand.Read(spanID)
	return "00-" + hex.EncodeToString(traceID) + "-" + hex.EncodeToString(spanID) + "-01"
}

// isValidTraceparent проверяет формат по W3C: ровно четыре hex-поля нужной длины,
// причём trace-id и span-id не должны быть полностью нулевыми.
func isValidTraceparent(tp string) bool {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		return false
	}
	if len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	if isAllZero(parts[1]) || isAllZero(parts[2]) {
		return false
	}
	for _, p := range parts {
		if _, err := hex.DecodeString(p); err != nil {
			return false
		}
	}
	return true
}

func isAllZero(s string) bool {
	return strings.Trim(s, "0") == ""
}
