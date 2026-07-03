package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/testutil"
)

// LogNotifier — заглушка: Send пишет уведомление в лог и не возвращает ошибку.
func TestLogNotifier_Send_LogsFieldsAndReturnsNil(t *testing.T) {
	logger, buf := testutil.CaptureLogger()
	n := NewLogNotifier(logger)

	err := n.Send(context.Background(), "user-42", Notification{
		Type:      NotifyBookingConfirmed,
		BookingID: "b-7",
		RoomID:    "room-3",
		Title:     "Бронирование подтверждено",
	})
	require.NoError(t, err)

	// Разбираем последнюю строку JSON-лога и проверяем поля.
	line := lastLogLine(t, buf.String())
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &rec))

	assert.Equal(t, "user-42", rec["user_id"])
	assert.Equal(t, NotifyBookingConfirmed, rec["notification_type"])
	assert.Equal(t, "b-7", rec["booking_id"])
}

func lastLogLine(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.NotEmpty(t, lines, "logger produced no output")
	return lines[len(lines)-1]
}
