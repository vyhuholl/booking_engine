package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/events"
)

// TestEvent_JSONContract фиксирует формат сообщения в шине: имена полей —
// snake_case, время в RFC3339. Ломать этот контракт нельзя без согласования
// с потребителями топика.
func TestEvent_JSONContract(t *testing.T) {
	ev := events.Event{
		EventID:   "evt-1",
		Type:      events.TypeBookingCreated,
		BookingID: "b-1",
		UserID:    "user-1",
		RoomID:    "room-1",
		Timestamp: time.Date(2026, 5, 19, 8, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(ev)
	assert.NoError(t, err)

	var got map[string]any
	assert.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, map[string]any{
		"event_id":   "evt-1",
		"type":       "booking.created",
		"booking_id": "b-1",
		"user_id":    "user-1",
		"room_id":    "room-1",
		"timestamp":  "2026-05-19T08:00:00Z",
	}, got)
}

func TestEventTypeConstants(t *testing.T) {
	assert.Equal(t, "booking.created", events.TypeBookingCreated)
	assert.Equal(t, "booking.cancelled", events.TypeBookingCancelled)
}
