package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/events"
	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/testutil"
)

// --- Моки -------------------------------------------------------------

type mockAdmins struct {
	admins []model.User
	err    error
}

func (m *mockAdmins) ListAdmins(_ context.Context) ([]model.User, error) {
	return m.admins, m.err
}

type mockDedup struct {
	processed map[string]bool
	err       error
}

func (m *mockDedup) IsProcessed(_ context.Context, eventID string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	return m.processed[eventID], nil
}

func (m *mockDedup) MarkProcessed(_ context.Context, eventID, _ string) error {
	if m.processed == nil {
		m.processed = make(map[string]bool)
	}
	m.processed[eventID] = true
	return nil
}

type mockDeadLetter struct {
	entries []deadLetterEntry
	err     error
}

func (m *mockDeadLetter) SaveDeadLetter(_ context.Context, eventID, userID, notifType, reason string) error {
	m.entries = append(m.entries, deadLetterEntry{eventID, userID, notifType, reason})
	return m.err
}

type mockBookings struct {
	booking model.Booking
	err     error
}

func (m *mockBookings) Get(_ context.Context, id string) (model.Booking, error) {
	if m.err != nil {
		return model.Booking{}, m.err
	}
	return m.booking, nil
}

type mockNotifier struct {
	calls  []sentNotification
	errFn  func(attempt int) error
	cancel chan struct{}
}

func (m *mockNotifier) Send(_ context.Context, userID string, n Notification) error {
	attempt := len(m.calls) + 1
	m.calls = append(m.calls, sentNotification{userID, n})
	if m.errFn != nil {
		return m.errFn(attempt)
	}
	return nil
}

// --- Тесты --------------------------------------------------------------

func TestDispatcher_Mapping_EachEventTypeToCorrectRecipients(t *testing.T) {
	owner := "user-owner"
	admin1 := model.User{ID: "user-admin-1", Role: model.RoleAdmin}
	admin2 := model.User{ID: "user-admin-2", Role: model.RoleAdmin}
	reason := "превышен лимит"

	tests := []struct {
		name                string
		ev                  events.Event
		wantRecipients      map[string]bool // set of userIDs who should get notification
		wantNotifType       string
		bookingsForRejected *model.Booking // для booking.rejected, если тестируем причину
	}{
		{
			name:           "booking.created → owner",
			ev:             testEvt("evt-1", events.TypeBookingCreated, "b-1", owner),
			wantRecipients: map[string]bool{owner: true},
			wantNotifType:  NotifyBookingConfirmed,
		},
		{
			name:           "booking.cancelled → owner",
			ev:             testEvt("evt-2", events.TypeBookingCancelled, "b-2", owner),
			wantRecipients: map[string]bool{owner: true},
			wantNotifType:  NotifyBookingCancelled,
		},
		{
			name:           "booking.approved → owner",
			ev:             testEvt("evt-3", events.TypeBookingApproved, "b-3", owner),
			wantRecipients: map[string]bool{owner: true},
			wantNotifType:  NotifyBookingApproved,
		},
		{
			name:                "booking.rejected → owner + reason",
			ev:                  testEvt("evt-4", events.TypeBookingRejected, "b-4", owner),
			wantRecipients:      map[string]bool{owner: true},
			wantNotifType:       NotifyBookingRejected,
			bookingsForRejected: &model.Booking{ID: "b-4", RejectionReason: &reason},
		},
		{
			name:           "booking.pending_approval → all admins",
			ev:             testEvt("evt-5", events.TypeBookingPendingApproval, "b-5", owner),
			wantRecipients: map[string]bool{admin1.ID: true, admin2.ID: true},
			wantNotifType:  NotifyApprovalRequested,
		},
		{
			name:           "unknown type → no recipients",
			ev:             testEvt("evt-6", "booking.teleported", "b-6", owner),
			wantRecipients: map[string]bool{},
			wantNotifType:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notifier := &mockNotifier{}
			admins := &mockAdmins{admins: []model.User{admin1, admin2}}
			dedup := &mockDedup{}
			dl := &mockDeadLetter{}

			var bookings *mockBookings
			if tt.bookingsForRejected != nil {
				bookings = &mockBookings{booking: *tt.bookingsForRejected}
			}

			logger, _ := testutil.CaptureLogger()
			cfg := Config{RetryMax: 1, RetryBase: time.Millisecond}
			d := NewDispatcher(notifier, admins, dedup, dl, cfg, logger)
			if bookings != nil {
				d = NewDispatcher(notifier, admins, dedup, dl, cfg, logger, WithBookingLookup(bookings))
			}

			err := d.Handle(context.Background(), tt.ev)
			require.NoError(t, err)

			// Проверяем адресатов.
			gotRecipients := make(map[string]bool, len(notifier.calls))
			for _, c := range notifier.calls {
				gotRecipients[c.userID] = true
				if tt.wantNotifType != "" {
					assert.Equal(t, tt.wantNotifType, c.n.Type)
				}
			}
			assert.Equal(t, tt.wantRecipients, gotRecipients)

			// Для rejected с причиной проверяем поле Reason.
			if tt.wantNotifType == NotifyBookingRejected && tt.bookingsForRejected != nil {
				require.Len(t, notifier.calls, 1)
				assert.Equal(t, reason, notifier.calls[0].n.Reason)
			}
		})
	}
}

func TestDispatcher_Dedup_SecondCallSkips(t *testing.T) {
	notifier := &mockNotifier{}
	admins := &mockAdmins{}
	dedup := &mockDedup{processed: map[string]bool{"evt-dup": true}}
	dl := &mockDeadLetter{}

	logger, _ := testutil.CaptureLogger()
	cfg := Config{RetryMax: 1, RetryBase: time.Millisecond}
	d := NewDispatcher(notifier, admins, dedup, dl, cfg, logger)

	ev := testEvt("evt-dup", events.TypeBookingCreated, "b-dup", "user-1")
	err := d.Handle(context.Background(), ev)
	require.NoError(t, err)

	assert.Empty(t, notifier.calls, "dedup event must not notify")
}

func TestDispatcher_Dedup_EmptyEventID_UsesSyntheticKey(t *testing.T) {
	notifier := &mockNotifier{}
	admins := &mockAdmins{}
	dedup := &mockDedup{processed: map[string]bool{}}
	dl := &mockDeadLetter{}

	logger, buf := testutil.CaptureLogger()
	cfg := Config{RetryMax: 1, RetryBase: time.Millisecond}
	d := NewDispatcher(notifier, admins, dedup, dl, cfg, logger)

	// Два события без EventID, но с одинаковыми остальными полями → дедуп сработает.
	ev := events.Event{Type: events.TypeBookingCreated, BookingID: "b-x", UserID: "user-1", RoomID: "room-1", Timestamp: testutil.FixedNow}

	err := d.Handle(context.Background(), ev)
	require.NoError(t, err)
	err = d.Handle(context.Background(), ev)
	require.NoError(t, err)

	assert.Len(t, notifier.calls, 1, "synthetic key dedup must work")
	assert.Contains(t, buf.String(), "event without event_id, using synthetic dedup key")
}

func TestDispatcher_Retry_SuccessOnSecondAttempt(t *testing.T) {
	attempts := 0
	notifier := &mockNotifier{errFn: func(i int) error {
		attempts = i
		if i == 1 {
			return errors.New("transient")
		}
		return nil
	}}
	admins := &mockAdmins{}
	dedup := &mockDedup{}
	dl := &mockDeadLetter{}

	logger, _ := testutil.CaptureLogger()
	cfg := Config{RetryMax: 3, RetryBase: time.Millisecond}
	d := NewDispatcher(notifier, admins, dedup, dl, cfg, logger)

	ev := testEvt("evt-retry", events.TypeBookingCreated, "b-retry", "user-1")
	err := d.Handle(context.Background(), ev)
	require.NoError(t, err)

	assert.Equal(t, 2, attempts, "must retry once then succeed")
	assert.Empty(t, dl.entries, "successful retry must not dead-letter")
}

func TestDispatcher_RetryExhausted_DeadLetters(t *testing.T) {
	notifier := &mockNotifier{errFn: func(int) error { return errors.New("permanent") }}
	admins := &mockAdmins{}
	dedup := &mockDedup{}
	dl := &mockDeadLetter{}

	logger, _ := testutil.CaptureLogger()
	cfg := Config{RetryMax: 3, RetryBase: time.Millisecond}
	d := NewDispatcher(notifier, admins, dedup, dl, cfg, logger)

	ev := testEvt("evt-dl", events.TypeBookingCreated, "b-dl", "user-1")
	err := d.Handle(context.Background(), ev)
	require.NoError(t, err, "dead-letter is not a fatal error (offset can be committed)")

	assert.Len(t, notifier.calls, 3, "must exhaust all retries")
	assert.Len(t, dl.entries, 1, "undelivered notification goes to dead-letter")
	assert.Equal(t, "evt-dl", dl.entries[0].eventID)
	assert.Equal(t, "user-1", dl.entries[0].userID)
	assert.Equal(t, NotifyBookingConfirmed, dl.entries[0].notificationType)
	assert.Contains(t, dl.entries[0].reason, "permanent")
}

func testEvt(eventID, typ, bookingID, userID string) events.Event {
	return events.Event{
		EventID:   eventID,
		Type:      typ,
		BookingID: bookingID,
		UserID:    userID,
		RoomID:    "room-1",
		Timestamp: testutil.FixedNow,
	}
}
