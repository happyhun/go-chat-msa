package natsbus

import (
	"encoding/json"
	"testing"
	"time"

	"go-chat-msa/internal/websocket/hub"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnect(t *testing.T) {
	t.Parallel()

	t.Run("Failure: url is empty", func(t *testing.T) {
		t.Parallel()

		bus, err := Connect(Config{Name: "websocket-service-test"})

		assert.ErrorIs(t, err, ErrURLRequired)
		assert.Nil(t, bus)
	})

	t.Run("Success: unreachable server keeps retrying instead of failing", func(t *testing.T) {
		t.Parallel()

		bus, err := Connect(Config{
			URL:             "nats://127.0.0.1:14222",
			Name:            "websocket-service-test",
			FlushTimeout:    10 * time.Millisecond,
			SubPendingMsgs:  1000,
			SubPendingBytes: 8 * 1024 * 1024,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = bus.Drain() })

		assert.False(t, bus.Connected())
		sub, err := bus.SubscribeEvents(func(hub.RoomEvent) {})
		require.NoError(t, err, "initial reconnect must retain the event subscription")
		require.NotNil(t, sub)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	})
}

func TestBus_NilSafety(t *testing.T) {
	t.Parallel()

	var bus *Bus

	assert.False(t, bus.Connected())
	assert.NoError(t, bus.Drain())
}

func TestEnvelopeFromMsg_MissingCustomHeaders(t *testing.T) {
	t.Parallel()
	roomID := uuid.NewString()
	msg := &nats.Msg{
		Header: nats.Header{"Nats-Time-Stamp": {time.Now().UTC().Format(time.RFC3339Nano)}},
		Data:   []byte(`{"id":"payload-id","sender_id":"payload-sender"}`),
	}

	got := envelopeFromMsg(roomID, msg)

	assert.Equal(t, roomID, got.RoomID)
	assert.Equal(t, msg.Data, got.Payload)
	assert.Empty(t, got.MessageID)
	assert.Empty(t, got.SenderID)
	assert.True(t, got.ReceivedAt.IsZero())
}

func TestRoomEventFromMsg(t *testing.T) {
	t.Parallel()

	roomID := uuid.NewString()
	event := hub.RoomEvent{Type: hub.EventRoomClosed, RoomID: roomID}
	payload, err := json.Marshal(event)
	require.NoError(t, err)

	tests := []struct {
		name    string
		message *nats.Msg
		valid   bool
	}{
		{name: "Success", message: &nats.Msg{Subject: subjectRoomEventPrefix + roomID, Data: payload}, valid: true},
		{name: "Failure: invalid payload", message: &nats.Msg{Subject: subjectRoomEventPrefix + roomID, Data: []byte("{")}},
		{name: "Failure: subject mismatch", message: &nats.Msg{Subject: subjectRoomEventPrefix + uuid.NewString(), Data: payload}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := roomEventFromMsg(tt.message)
			assert.Equal(t, tt.valid, ok)
			if ok {
				assert.Equal(t, event, got)
			}
		})
	}
}
