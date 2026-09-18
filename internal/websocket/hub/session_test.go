package hub

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSession(conn *websocket.Conn, senderID, roomID string, unregisterCh chan<- *session) *session {
	return newSession(uuid.NewString(), sessionConfig{
		writeWait: 10 * time.Second, pongWait: 60 * time.Second, pingPeriod: 54 * time.Second,
	}, conn, senderID, roomID, unregisterCh, nil, nil)
}
func TestSession_ReadPump(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := createTestWSPair(t)
	defer func() { _ = clientConn.Close() }()
	published := make(chan *Message, 1)
	s := newSession(uuid.NewString(), sessionConfig{writeWait: 10 * time.Second, pongWait: 60 * time.Second, pingPeriod: 54 * time.Second}, serverConn, "user", "room", make(chan *session, 1), func(_ context.Context, msg *Message) error {
		published <- msg
		return nil
	}, nil)
	go s.readPump(t.Context())
	require.NoError(t, clientConn.WriteJSON(map[string]string{"content": "hello", "client_msg_id": uuid.NewString()}))
	select {
	case msg := <-published:
		assert.Equal(t, "hello", msg.Content)
		assert.Equal(t, msgTypeChat, msg.Type)
	case <-time.After(time.Second):
		t.Fatal("message was not published")
	}
}

func TestSession_ReadPump_Unregister(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := createTestWSPair(t)
	unregisterCh := make(chan *session, 1)
	s := newTestSession(serverConn, "user1", "room1", unregisterCh)

	go s.run(t.Context())

	require.NoError(t, clientConn.Close())

	select {
	case closed := <-unregisterCh:
		assert.Equal(t, s, closed)
	case <-time.After(time.Second):
		t.Fatal("unregister should be called after connection close")
	}
}

func TestSession_WritePumpPreservesPayload(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := createTestWSPair(t)
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()
	s := newTestSession(serverConn, "user", "room", nil)
	go s.writePump(t.Context())
	payload := []byte(`{"id":"m1","content":"hi } \"there\""}`)
	original := string(payload)
	for range 3 {
		s.send(t.Context(), egressPacket{data: payload})
		require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(time.Second)))
		messageType, data, err := clientConn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, websocket.TextMessage, messageType)
		assert.Equal(t, original, string(data))
		assert.Equal(t, original, string(payload))
	}
}

func TestSession_SendClosesConnectionWhenBufferFull(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := createTestWSPair(t)
	defer func() { _ = clientConn.Close() }()
	unregisterCh := make(chan *session, 1)
	s := newTestSession(serverConn, "user", "room", unregisterCh)
	packet := egressPacket{data: []byte(`{"content":"queued"}`)}
	for range sendBufferSize {
		s.send(t.Context(), packet)
	}
	require.False(t, s.isClosed())
	s.send(t.Context(), packet)
	require.True(t, s.isClosed())
	s.send(t.Context(), packet)
	s.close()
	assert.Len(t, s.sendCh, sendBufferSize)
	require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := clientConn.ReadMessage()
	assert.True(t, websocket.IsCloseError(err, websocket.CloseAbnormalClosure), "expected connection closure without draining queued frames: %v", err)
	go s.run(t.Context())
	select {
	case got := <-unregisterCh:
		assert.Same(t, s, got)
	case <-time.After(time.Second):
		t.Fatal("overflowed session was not unregistered")
	}
}

func TestSession_ConcurrentOverflowAndClose(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := createTestWSPair(t)
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()
	s := newTestSession(serverConn, "user", "room", nil)
	packet := egressPacket{data: []byte(`{"content":"queued"}`)}
	for range sendBufferSize {
		s.send(t.Context(), packet)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { s.send(t.Context(), packet) })
		wg.Go(s.close)
	}
	wg.Wait()
	assert.True(t, s.isClosed())
}

func TestSession_WritePumpPing(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := createTestWSPair(t)
	defer func() { _ = clientConn.Close() }()

	s := newSession(uuid.NewString(), sessionConfig{
		writeWait:  10 * time.Second,
		pongWait:   60 * time.Second,
		pingPeriod: 50 * time.Millisecond,
	}, serverConn, "user4", "room4", nil, nil, nil)
	go s.writePump(context.Background())

	var receivedPing bool
	clientConn.SetPingHandler(func(string) error {
		receivedPing = true
		return nil
	})
	require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, _, err := clientConn.ReadMessage()
	assert.Error(t, err, "read should end at the deadline after handling the ping")

	assert.True(t, receivedPing, "ping message should be received")
}

func TestSession_Run(t *testing.T) {
	t.Parallel()

	t.Run("Success: Run 루프 활성화 중 메시지 전송", func(t *testing.T) {
		t.Parallel()
		serverConn, clientConn := createTestWSPair(t)
		defer func() { _ = clientConn.Close() }()

		unregisterCh := make(chan *session, 1)
		s := newTestSession(serverConn, "u3", "r3", unregisterCh)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go s.run(ctx)

		s.send(context.Background(), egressPacket{data: []byte(`{"content":"hello"}`)})
		require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(time.Second)))
		_, data, err := clientConn.ReadMessage()
		require.NoError(t, err)
		assert.Contains(t, string(data), `"content":"hello"`)
	})

	t.Run("Success: 컨텍스트 취소 시 세션 리소스 정리", func(t *testing.T) {
		t.Parallel()
		serverConn, clientConn := createTestWSPair(t)
		defer func() { _ = clientConn.Close() }()

		unregisterCh := make(chan *session, 1)
		s := newTestSession(serverConn, "u4", "r4", unregisterCh)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			s.run(ctx)
			close(done)
		}()

		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("session.run should return after context cancel")
		}
	})
}

type blockedWriteConn struct {
	net.Conn
	blockWrites  atomic.Bool
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
}

func (c *blockedWriteConn) Write(p []byte) (int, error) {
	if !c.blockWrites.Load() {
		return c.Conn.Write(p)
	}
	c.writeOnce.Do(func() { close(c.writeStarted) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockedWriteConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestSession_OverflowUnblocksRunningWriter(t *testing.T) {
	t.Parallel()
	peerCh := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			peerCh <- conn
		}
	}))
	defer server.Close()
	var blocked *blockedWriteConn
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		blocked = &blockedWriteConn{Conn: conn, writeStarted: make(chan struct{}), closed: make(chan struct{})}
		return blocked, nil
	}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer conn.Close()
	peer := <-peerCh
	defer peer.Close()
	blocked.blockWrites.Store(true)
	unregisterCh := make(chan *session, 1)
	s := newTestSession(conn, "slow", "room", unregisterCh)
	done := make(chan struct{})
	go func() {
		s.run(t.Context())
		close(done)
	}()
	packet := egressPacket{data: []byte(`{"content":"queued"}`)}
	s.send(t.Context(), packet)
	select {
	case <-blocked.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach the blocked connection")
	}
	for range sendBufferSize {
		s.send(t.Context(), packet)
	}
	require.False(t, s.isClosed())
	s.send(t.Context(), packet)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("overflow did not stop both session pumps")
	}
	assert.Same(t, s, <-unregisterCh)
	assert.Len(t, s.sendCh, sendBufferSize, "queued frames must not be drained")
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err = peer.ReadMessage()
	assert.True(t, websocket.IsCloseError(err, websocket.CloseAbnormalClosure))
}
