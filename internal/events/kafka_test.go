package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/tracing"
)

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func sampleEvent() Event {
	return Event{
		Type:      TypeBookingCreated,
		BookingID: "b-1",
		UserID:    "user-1",
		RoomID:    "room-1",
	}
}

// TestNewMessage_CarriesTraceparentInHeaderNotBody фиксирует контракт продюсера:
// trace-контекст уходит в заголовок сообщения (kafka.Header), а тело Event его
// не содержит — консьюмер восстанавливает трейс из заголовков.
func TestNewMessage_CarriesTraceparentInHeaderNotBody(t *testing.T) {
	ctx := tracing.WithTraceparent(context.Background(), testTraceparent)

	msg, err := newMessage(ctx, "booking.events", sampleEvent())
	require.NoError(t, err)

	// Ключ сообщения — BookingID (порядок событий одной брони в одном партишне).
	assert.Equal(t, []byte("b-1"), msg.Key)

	// traceparent — в заголовке.
	assert.Equal(t, testTraceparent, headerValue(msg, tracing.HeaderName))

	// ...и не протёк в тело Event.
	var body map[string]any
	require.NoError(t, json.Unmarshal(msg.Value, &body))
	assert.NotContains(t, body, "traceparent")
	assert.NotContains(t, body, "trace_id")
}

// TestContextFromMessage_RestoresTraceContext — сторона консьюмера: контекст
// восстанавливается из заголовков сообщения, trace_id совпадает с продюсерским.
func TestContextFromMessage_RestoresTraceContext(t *testing.T) {
	producerCtx := tracing.WithTraceparent(context.Background(), testTraceparent)
	msg, err := newMessage(producerCtx, "booking.events", sampleEvent())
	require.NoError(t, err)

	// Консьюмер стартует с чистым контекстом и поднимает трейс из заголовков.
	consumerCtx := ContextFromMessage(context.Background(), msg)

	assert.Equal(t, testTraceparent, tracing.Traceparent(consumerCtx))
	assert.Equal(t, tracing.TraceID(producerCtx), tracing.TraceID(consumerCtx))
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", tracing.TraceID(consumerCtx))
}

// TestNewMessage_NoTraceparentNoHeader — без trace-контекста в ctx заголовок не
// добавляется, а ContextFromMessage не выдумывает трейс.
func TestNewMessage_NoTraceparentNoHeader(t *testing.T) {
	msg, err := newMessage(context.Background(), "booking.events", sampleEvent())
	require.NoError(t, err)

	assert.Empty(t, msg.Headers)
	assert.Empty(t, tracing.Traceparent(ContextFromMessage(context.Background(), msg)))
}

func headerValue(msg kafka.Message, key string) string {
	for _, h := range msg.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
