//go:build integration

package natsbus

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-chat-msa/internal/websocket/hub"

	"github.com/google/uuid"
	containerapi "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	natscontainer "github.com/testcontainers/testcontainers-go/modules/nats"
)

func TestPublishMessage_RePublishPreservesHeaders(t *testing.T) {
	ctx := t.Context()
	container, err := natscontainer.Run(ctx, "nats:2.14.6-alpine",
		natscontainer.WithConfigFile(strings.NewReader("port: 4222\njetstream { store_dir: /data/jetstream }\n")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)
	bus, err := Connect(Config{URL: url, Name: t.Name(), FlushTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Drain() })
	_, err = bus.js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "CHAT_PERSIST", Subjects: []string{"chat.persist.*"},
		Storage: jetstream.FileStorage, Retention: jetstream.WorkQueuePolicy,
		RePublish: &jetstream.RePublish{Source: "chat.persist.*", Destination: "room.msg.$1"},
	})
	require.NoError(t, err)
	want := hub.Envelope{
		RoomID: uuid.NewString(), MessageID: uuid.NewString(), SenderID: "sender",
		OriginPod: "pod-a", ReceivedAt: time.Now().Add(-time.Minute),
		Payload:    []byte(`{"content":"header preservation"}`),
		Persistent: true, DedupID: uuid.NewString(),
	}
	rawSub, err := bus.conn.SubscribeSync(subjectRoomMsgPrefix + want.RoomID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rawSub.Unsubscribe() })
	received := make(chan hub.Envelope, 1)
	sub, err := bus.SubscribeRoom(ctx, want.RoomID, func(env hub.Envelope) { received <- env })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, bus.PublishMessage(ctx, want))
	raw, err := rawSub.NextMsg(5 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, want.MessageID, raw.Header.Get(headerMessageID))
	assert.Equal(t, want.SenderID, raw.Header.Get(headerSenderID))
	assert.Equal(t, want.OriginPod, raw.Header.Get(headerOriginPod))
	assert.Equal(t, strconv.FormatInt(want.ReceivedAt.UnixNano(), 10), raw.Header.Get(headerReceivedAt))
	assert.Equal(t, "CHAT_PERSIST", raw.Header.Get("Nats-Stream"))
	assert.Equal(t, want.Payload, raw.Data)
	select {
	case got := <-received:
		assert.Equal(t, want.RoomID, got.RoomID)
		assert.Equal(t, want.MessageID, got.MessageID)
		assert.Equal(t, want.SenderID, got.SenderID)
		assert.Equal(t, want.OriginPod, got.OriginPod)
		assert.True(t, want.ReceivedAt.Equal(got.ReceivedAt))
		assert.Equal(t, want.Payload, got.Payload)
	case <-time.After(5 * time.Second):
		t.Fatal("republished message was not delivered")
	}
}

func TestEventSubscriptionSurvivesInitialConnectionFailure(t *testing.T) {
	ctx := t.Context()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	hostPort := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	natsURL := fmt.Sprintf("nats://127.0.0.1:%d", hostPort)

	bus, err := Connect(Config{
		URL:          natsURL,
		Name:         "initial-reconnect-test",
		FlushTimeout: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	received := make(chan hub.RoomEvent, 1)
	sub, err := bus.SubscribeEvents(func(event hub.RoomEvent) { received <- event })
	require.NoError(t, err)

	var container *natscontainer.NATSContainer
	t.Cleanup(func() {
		_ = sub.Unsubscribe()
		_ = bus.Drain()
		if container != nil {
			_ = container.Terminate(context.Background())
		}
	})

	container, err = natscontainer.Run(ctx, "nats:2.14.6-alpine",
		testcontainers.WithHostConfigModifier(func(hostConfig *containerapi.HostConfig) {
			if hostConfig.PortBindings == nil {
				hostConfig.PortBindings = make(network.PortMap)
			}
			hostConfig.PortBindings[network.MustParsePort("4222/tcp")] = []network.PortBinding{{
				HostIP:   netip.MustParseAddr("127.0.0.1"),
				HostPort: strconv.Itoa(hostPort),
			}}
		}),
	)
	require.NoError(t, err)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.True(collect, bus.Connected(), "status=%s last_error=%v", bus.conn.Status(), bus.conn.LastError())
	}, 30*time.Second, 100*time.Millisecond)

	want := hub.RoomEvent{
		Type:       hub.EventRoomClosed,
		RoomID:     uuid.NewString(),
		OccurredAt: time.Now(),
	}
	require.NoError(t, bus.PublishEvent(ctx, want))

	select {
	case got := <-received:
		require.Equal(t, want.Type, got.Type)
		require.Equal(t, want.RoomID, got.RoomID)
	case <-time.After(5 * time.Second):
		t.Fatal("event subscription was not restored after the initial connection succeeded")
	}
}
