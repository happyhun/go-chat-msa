package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSessionConfig() sessionConfig {
	return sessionConfig{
		writeWait:  10 * time.Second,
		pongWait:   60 * time.Second,
		pingPeriod: 54 * time.Second,
	}
}

func createTestWSPair(t *testing.T) (serverConn, clientConn *websocket.Conn) {
	t.Helper()
	connCh := make(chan *websocket.Conn, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- c
	}))
	t.Cleanup(s.Close)
	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
	cc, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	return <-connCh, cc
}

func registerTestSession(t *testing.T, h *Hub, userID string) *websocket.Conn {
	t.Helper()
	serverConn, clientConn := createTestWSPair(t)
	err := h.register(t.Context(), serverConn, userID)
	require.NoError(t, err)
	return clientConn
}

func newTestHub(roomID string) *Hub {
	return newHub(roomID, testSessionConfig(), time.Minute, nil, nil)
}

func TestHub_ExpiresWithoutRegistration(t *testing.T) {
	h := newHub("room", testSessionConfig(), 10*time.Millisecond, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.run(ctx)
	select {
	case <-h.done():
		require.True(t, h.isDraining())
	case <-time.After(time.Second):
		t.Fatal("hub without a registered session did not expire")
	}
}

func testPacket(messageID, senderID, content string) deliverPacket {
	return deliverPacket{
		payload:    []byte(`{"id":"` + messageID + `","content":"` + content + `"}`),
		messageID:  messageID,
		senderID:   senderID,
		receivedAt: time.Now(),
		arrivedAt:  time.Now(),
	}
}

func TestHub_FanOutToAllSessions(t *testing.T) {
	t.Parallel()

	h := newTestHub("room1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.run(ctx)

	client1 := registerTestSession(t, h, "user1")
	client2 := registerTestSession(t, h, "user2")

	h.deliver(ctx, testPacket("01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d8e", "user1", "hello world"))

	for _, cc := range []*websocket.Conn{client1, client2} {
		require.NoError(t, cc.SetReadDeadline(time.Now().Add(time.Second)))
		_, data, err := cc.ReadMessage()
		require.NoError(t, err)

		var frame struct {
			Content string `json:"content"`
		}
		require.NoError(t, json.Unmarshal(data, &frame))
		assert.Equal(t, "hello world", frame.Content)
		assert.NotContains(t, string(data), `"frame_no"`)
	}
}

func TestHub_AllowsMultipleSessionsPerUser(t *testing.T) {
	t.Parallel()

	h := newTestHub("multi-room")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.run(ctx)

	first := registerTestSession(t, h, "same-user")
	second := registerTestSession(t, h, "same-user")

	h.deliver(ctx, testPacket("01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d8f", "other", "broadcast"))

	for _, cc := range []*websocket.Conn{first, second} {
		require.NoError(t, cc.SetReadDeadline(time.Now().Add(time.Second)))
		_, data, err := cc.ReadMessage()
		require.NoError(t, err, "both sessions of the same user must receive the message")
		assert.Contains(t, string(data), "broadcast")
	}
}

func TestHub_ObserveOrder(t *testing.T) {
	t.Parallel()

	const (
		older = "01920f6a-7c3e-7b1a-9d2f-000000000001"
		newer = "01920f6b-7c3e-7b1a-9d2f-000000000002"
	)

	h := newTestHub("order-room")

	h.observeOrder(t.Context(), newer)
	last := h.lastDelivered
	require.Equal(t, newer, last)

	h.observeOrder(t.Context(), older)
	last = h.lastDelivered
	assert.Equal(t, newer, last, "역전된 id는 커서를 되돌리지 않는다")
}

func TestHub_Lifecycle(t *testing.T) {
	t.Parallel()

	t.Run("Failure: 종료된 Hub에 세션 등록 시도 시 에러 반환", func(t *testing.T) {
		t.Parallel()
		h := newTestHub("stop-room")
		close(h.doneCh)
		err := h.register(t.Context(), nil, "user1")
		require.Error(t, err)
		assert.ErrorContains(t, err, "hub closed")
	})

	t.Run("Success: 종료된 Hub에 전달 시 패닉 방지", func(t *testing.T) {
		t.Parallel()
		h := newTestHub("stop-room")
		close(h.doneCh)
		h.deliver(t.Context(), testPacket("01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d80", "u", "dropped"))
	})

	t.Run("Success: shutdown 시 모든 세션 정리", func(t *testing.T) {
		t.Parallel()
		h := newTestHub("shutdown-room")
		go func() {
			serverConn, _ := createTestWSPair(t)
			_ = h.register(t.Context(), serverConn, "user1")
		}()

		time.Sleep(50 * time.Millisecond)
		h.shutdown()
		assert.Empty(t, h.sessions)
	})

	t.Run("Success: 방 세션 정리 시 1013 close frame 전송", func(t *testing.T) {
		t.Parallel()
		h := newTestHub("closed-room")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go h.run(ctx)

		clientConn := registerTestSession(t, h, "user1")
		defer func() { _ = clientConn.Close() }()

		h.forceClose(closeCodeTryAgainLater, closeReasonRoomClosed)

		require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(time.Second)))
		_, _, err := clientConn.ReadMessage()
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
		assert.Equal(t, closeCodeTryAgainLater, closeErr.Code)
		assert.Equal(t, closeReasonRoomClosed, closeErr.Text)
	})
}

func TestHub_OverflowClosesOnlySlowSession(t *testing.T) {
	t.Parallel()
	h := newTestHub("room")
	slowServer, slowClient := createTestWSPair(t)
	defer func() { _ = slowClient.Close() }()
	fastServer, fastClient := createTestWSPair(t)
	defer func() { _ = fastClient.Close() }()
	defer func() { _ = fastServer.Close() }()
	slow := newTestSession(slowServer, "slow", "room", nil)
	fast := newTestSession(fastServer, "fast", "room", nil)
	h.sessions[slow.id] = slow
	h.sessions[fast.id] = fast
	for range sendBufferSize {
		slow.send(t.Context(), egressPacket{data: []byte(`{"content":"queued"}`)})
	}
	go fast.writePump(t.Context())
	packet := testPacket("01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d8e", "sender", "last message")
	h.fanOut(t.Context(), packet)
	require.True(t, slow.isClosed())
	require.False(t, fast.isClosed())
	require.NoError(t, fastClient.SetReadDeadline(time.Now().Add(time.Second)))
	_, data, err := fastClient.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, packet.payload, data)
	require.NoError(t, slowClient.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err = slowClient.ReadMessage()
	assert.True(t, websocket.IsCloseError(err, websocket.CloseAbnormalClosure))
}
