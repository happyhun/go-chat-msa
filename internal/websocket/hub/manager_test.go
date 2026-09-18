package hub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go-chat-msa/internal/shared/config"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testManagerConfig() config.ManagerConfig {
	return config.ManagerConfig{
		WriteWait:   10 * time.Second,
		PongWait:    60 * time.Second,
		PingPeriod:  54 * time.Second,
		IdleTimeout: 5 * time.Minute,
	}
}

type fakeSubscription struct {
	unsubscribed bool
}

func (f *fakeSubscription) Unsubscribe() error {
	f.unsubscribed = true
	return nil
}

type fakeBus struct {
	mu sync.Mutex

	published []Envelope
	events    []RoomEvent
	roomSubs  map[string]func(Envelope)
	eventSub  func(RoomEvent)

	subscribeErr error
	publishErr   error
	connected    bool
}

func newFakeBus() *fakeBus {
	return &fakeBus{roomSubs: make(map[string]func(Envelope)), connected: true}
}

func (f *fakeBus) PublishMessage(_ context.Context, env Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, env)
	return nil
}

func (f *fakeBus) SubscribeRoom(_ context.Context, roomID string, deliver func(Envelope)) (Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	f.roomSubs[roomID] = deliver
	return &fakeSubscription{}, nil
}

func (f *fakeBus) PublishEvent(_ context.Context, ev RoomEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return f.publishErr
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeBus) SubscribeEvents(deliver func(RoomEvent)) (Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	f.eventSub = deliver
	return &fakeSubscription{}, nil
}

func (f *fakeBus) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeBus) publishedEnvelopes() []Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Envelope(nil), f.published...)
}

func (f *fakeBus) publishedEvents() []RoomEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RoomEvent(nil), f.events...)
}

func (f *fakeBus) hasRoomSubscription(roomID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.roomSubs[roomID]
	return ok
}

func startManager(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func newTestManager(t *testing.T, bus MessageBus) *Manager {
	t.Helper()
	m := NewManager(testManagerConfig(), config.RateLimitConfig{RPS: 1, Burst: 1, TTL: time.Minute}, bus, "test-pod", 2*time.Second, 100*time.Millisecond)
	startManager(t, m)
	return m
}

func TestManager_PrepareRegisterSubscribesRoom(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	m := newTestManager(t, bus)

	registration, err := m.PrepareRegister(t.Context(), "room-1")
	require.NoError(t, err)
	require.NotNil(t, registration)
	assert.True(t, bus.hasRoomSubscription("room-1"), "구독 반영 뒤에만 등록이 준비된다")

	registration.Cancel()
}

func TestManager_PrepareRegisterFailsWhenSubscribeFails(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	bus.subscribeErr = errors.New("flush timeout")
	m := newTestManager(t, bus)

	registration, err := m.PrepareRegister(t.Context(), "room-1")

	require.Nil(t, registration)
	require.ErrorIs(t, err, ErrRoomUnavailable)
}

func TestManager_PrepareRegisterFailsWhenBusDisconnected(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	bus.connected = false
	m := newTestManager(t, bus)

	registration, err := m.PrepareRegister(t.Context(), "room-1")

	require.Nil(t, registration)
	require.ErrorIs(t, err, ErrBusUnavailable)
}

func TestManager_PublishMessage(t *testing.T) {
	t.Parallel()
	t.Run("Success: system event fan-out", func(t *testing.T) {
		t.Parallel()
		bus := newFakeBus()
		m := newTestManager(t, bus)
		require.NoError(t, m.PublishSystemMessage(t.Context(), "room-1", "hello"))
		published := bus.publishedEnvelopes()
		require.Len(t, published, 1)
		assert.NotEmpty(t, published[0].MessageID)
		assert.Contains(t, string(published[0].Payload), `"content":"hello"`)
	})
	t.Run("Failure: Core bus unavailable", func(t *testing.T) {
		t.Parallel()
		bus := newFakeBus()
		bus.publishErr = errors.New("disconnected")
		m := newTestManager(t, bus)
		require.ErrorIs(t, m.PublishSystemMessage(t.Context(), "room-1", "hello"), ErrBusUnavailable)
	})
}

func TestManager_PublishSystemMessage(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	m := newTestManager(t, bus)

	require.NoError(t, m.PublishSystemMessage(t.Context(), "room-1", "alice님이 들어왔습니다."))

	published := bus.publishedEnvelopes()
	require.Len(t, published, 1)
	assert.Equal(t, systemSenderID, published[0].SenderID)
	assert.Contains(t, string(published[0].Payload), "들어왔습니다")
}

func TestManager_CloseRoomSessionsPublishesEvent(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	m := newTestManager(t, bus)

	require.NoError(t, m.CloseRoomSessions(t.Context(), "room-1"))

	events := bus.publishedEvents()
	require.Len(t, events, 1)
	assert.Equal(t, EventRoomClosed, events[0].Type)
	assert.Equal(t, "room-1", events[0].RoomID)
}

func TestManager_BusObserver(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	m := newTestManager(t, bus)
	releaseClose := make(chan struct{})
	m.sessionCloseDelay = func() time.Duration {
		<-releaseClose
		return 0
	}

	assert.True(t, m.BusConnected())

	m.OnDisconnected()
	assert.False(t, m.BusConnected(), "끊기면 readiness가 실패해야 한다")

	m.OnReconnected()
	assert.False(t, m.BusConnected(), "기존 세션을 닫기 전에는 새 연결을 받지 않아야 한다")
	close(releaseClose)
	require.Eventually(t, m.BusConnected, time.Second, 10*time.Millisecond)
}

func TestNewSystemMessage(t *testing.T) {
	t.Parallel()

	msg, err := NewSystemMessage("test-room", "hello")
	require.NoError(t, err)
	assert.Equal(t, "test-room", msg.RoomID)
	assert.Equal(t, "hello", msg.Content)
	assert.Equal(t, msgTypeSystem, msg.Type)
	assert.Equal(t, systemSenderID, msg.SenderID)
	assert.NotEmpty(t, msg.ClientMsgID)
}

func TestManager_ReconnectWaitsForSessionPumps(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, newFakeBus())
	m.sessionCloseDelay = func() time.Duration { return 0 }
	registration, err := m.PrepareRegister(t.Context(), "room-1")
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	registration.hub.publish = func(context.Context, *Message) error {
		close(started)
		<-release
		return nil
	}
	server, client := createTestWSPair(t)
	defer client.Close()
	require.NoError(t, registration.Commit(t.Context(), server, "user-1"))
	require.NoError(t, client.WriteJSON(map[string]string{"content": "hello", "client_msg_id": "00000000-0000-4000-8000-000000000001"}))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publish did not start")
	}
	other, err := m.PrepareRegister(t.Context(), "room-2")
	require.NoError(t, err)
	otherServer, otherClient := createTestWSPair(t)
	defer otherClient.Close()
	require.NoError(t, other.Commit(t.Context(), otherServer, "user-2"))
	pending, err := m.PrepareRegister(t.Context(), "room-3")
	require.NoError(t, err)
	m.OnDisconnected()
	m.OnReconnected()
	require.Eventually(t, registration.hub.isDraining, time.Second, time.Millisecond)
	assert.False(t, m.BusConnected())
	_, err = m.PrepareRegister(t.Context(), "room-4")
	require.ErrorIs(t, err, ErrBusUnavailable)
	require.ErrorIs(t, pending.Commit(t.Context(), nil, "user-3"), ErrBusUnavailable)
	select {
	case <-registration.hub.done():
		t.Fatal("hub finished before its session pump")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, m.BusConnected, time.Second, time.Millisecond)
	for _, h := range []*Hub{registration.hub, other.hub, pending.hub} {
		select {
		case <-h.done():
		default:
			t.Fatal("readiness restored before every old hub stopped")
		}
	}
	for _, conn := range []*websocket.Conn{client, otherClient} {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
		_, _, err := conn.ReadMessage()
		require.Error(t, err)
	}
	next, err := m.PrepareRegister(t.Context(), "room-1")
	require.NoError(t, err)
	require.NotSame(t, registration.hub, next.hub)
	next.Cancel()
}
