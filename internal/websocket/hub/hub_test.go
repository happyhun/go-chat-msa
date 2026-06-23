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

type mockStore struct {
	lastSeq   int64
	persisted []*Message
}

func (m *mockStore) GetLastSequenceNumber(_ context.Context, _ string) (int64, error) {
	return m.lastSeq, nil
}

func (m *mockStore) SaveMany(_ context.Context, msgs []*Message) error {
	m.persisted = append(m.persisted, msgs...)
	return nil
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

func TestHub_SequenceManagement(t *testing.T) {
	t.Parallel()

	t.Run("Success: 메시지 브로드캐스트 시 시퀀스 번호 자동 증가", func(t *testing.T) {
		t.Parallel()
		roomID := "test-room"
		store := &mockStore{lastSeq: 100}
		h := newHub(roomID, testSessionConfig(), 5*time.Minute, store, nil, time.Second, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, h.initializeSequence(ctx))
		go h.run(ctx)

		clientConn := registerTestSession(t, h, "user1")

		assert.Equal(t, int64(100), h.lastSequence.Load())

		msg := &Message{
			RoomID:   roomID,
			SenderID: "user1",
			Content:  "Hello",
			Type:     "chat",
		}
		h.broadcast(ctx, msg)

		clientConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, data, err := clientConn.ReadMessage()
		require.NoError(t, err)

		var received Message
		err = json.Unmarshal(data, &received)
		assert.NoError(t, err)
		assert.Equal(t, int64(101), received.SequenceNumber)
		assert.Equal(t, int64(101), h.lastSequence.Load())
	})
}

func TestHub_InitializeSequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		store     MessageStore
		expected  int64
		wantError bool
	}{
		{
			name:     "Success: DB에서 마지막 시퀀스 번호 정상 로드",
			store:    &mockStore{lastSeq: 50},
			expected: 50,
		},
		{
			name:      "Failure: DB 에러 발생 시 초기화 실패",
			store:     &errStore{},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHub("room1", testSessionConfig(), time.Minute, tt.store, nil, time.Second, nil)
			err := h.initializeSequence(t.Context())
			if tt.wantError {
				require.ErrorIs(t, err, ErrRoomSequenceUnavailable)
				assert.Equal(t, int64(0), h.lastSequence.Load())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, h.lastSequence.Load())
		})
	}
}

func TestHub_Lifecycle(t *testing.T) {
	t.Parallel()

	t.Run("Failure: 종료된 Hub에 세션 등록 시도 시 에러 반환", func(t *testing.T) {
		t.Parallel()
		h := newHub("stop-room", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
		close(h.doneCh)
		err := h.register(t.Context(), nil, "user1")
		require.Error(t, err)
		assert.ErrorContains(t, err, "hub closed")
	})

	t.Run("Success: 종료된 Hub에 브로드캐스트 시 패닉 방지", func(t *testing.T) {
		t.Parallel()
		h := newHub("stop-room", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
		close(h.doneCh)
		h.broadcast(t.Context(), &Message{RoomID: "stop-room"})
	})

	t.Run("Success: Shutdown 호출 시 모든 세션 정리", func(t *testing.T) {
		t.Parallel()
		h := newHub("shutdown-room", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
		go func() {
			serverConn, _ := createTestWSPair(t)
			_ = h.register(t.Context(), serverConn, "user1")
		}()

		time.Sleep(50 * time.Millisecond)
		h.shutdown()
		assert.Empty(t, h.sessions)
	})

	t.Run("Success: Handoff 종료 시 1012 close frame 전송", func(t *testing.T) {
		t.Parallel()
		h := newHub("handoff-room", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go h.run(ctx)

		clientConn := registerTestSession(t, h, "user1")
		defer clientConn.Close()

		h.forceClose(handoffCloseReason)

		clientConn.SetReadDeadline(time.Now().Add(time.Second))
		_, _, err := clientConn.ReadMessage()
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
		assert.Equal(t, handoffCloseCode, closeErr.Code)
		assert.Equal(t, handoffCloseReason, closeErr.Text)
	})
}

func TestHub_DrainWaitsForPersistenceAck(t *testing.T) {
	t.Parallel()

	persistCh := make(chan *persistTask, 1)
	h := newHub("drain-room", testSessionConfig(), time.Minute, &mockStore{}, persistCh, time.Second, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.run(ctx)

	h.broadcast(t.Context(), &Message{
		RoomID:   "drain-room",
		SenderID: "user-1",
		Content:  "hello",
		Type:     "chat",
	})

	var task *persistTask
	select {
	case task = <-persistCh:
	case <-time.After(time.Second):
		t.Fatal("persist task was not enqueued")
	}

	h.forceClose()

	select {
	case <-h.done():
		t.Fatal("hub stopped before persistence ack")
	case <-time.After(50 * time.Millisecond):
	}

	task.ack(nil)

	select {
	case <-h.done():
	case <-time.After(time.Second):
		t.Fatal("hub did not stop after persistence ack")
	}
}

func TestHub_DrainTimeoutLeavesPendingPersist(t *testing.T) {
	t.Parallel()

	persistCh := make(chan *persistTask, 1)
	h := newHub("drain-timeout-room", testSessionConfig(), time.Minute, &mockStore{}, persistCh, 20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.run(ctx)

	h.broadcast(t.Context(), &Message{
		RoomID:   "drain-timeout-room",
		SenderID: "user-1",
		Content:  "hello",
		Type:     "chat",
	})

	var task *persistTask
	select {
	case task = <-persistCh:
	case <-time.After(time.Second):
		t.Fatal("persist task was not enqueued")
	}

	h.forceClose()

	select {
	case <-h.done():
	case <-time.After(time.Second):
		t.Fatal("hub did not stop after drain timeout")
	}

	assert.Equal(t, int64(1), h.pendingPersist.Load())
	task.ack(nil)
}

func TestHub_PersistAckErrorMarksPersistFailed(t *testing.T) {
	t.Parallel()

	persistCh := make(chan *persistTask, 1)
	h := newHub("persist-error-room", testSessionConfig(), time.Minute, &mockStore{}, persistCh, time.Second, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.run(ctx)

	h.broadcast(t.Context(), &Message{
		RoomID:   "persist-error-room",
		SenderID: "user-1",
		Content:  "hello",
		Type:     "chat",
	})

	var task *persistTask
	select {
	case task = <-persistCh:
	case <-time.After(time.Second):
		t.Fatal("persist task was not enqueued")
	}

	task.ack(assert.AnError)

	require.Eventually(t, func() bool {
		return h.persistFailed.Load() && h.pendingPersist.Load() == 0
	}, time.Second, 10*time.Millisecond)
}

func TestHub_BroadcastUnblocksWhenDrainBeginsWithFullQueue(t *testing.T) {
	t.Parallel()

	h := newHub("drain-room", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
	for range cap(h.broadcastCh) {
		h.broadcastCh <- &Message{RoomID: "drain-room"}
	}

	done := make(chan struct{})
	go func() {
		h.broadcast(t.Context(), &Message{RoomID: "drain-room", Content: "after-drain"})
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	h.beginDrain()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast should unblock once drain begins")
	}
	assert.Len(t, h.broadcastCh, cap(h.broadcastCh))
}

type errStore struct {
	mockStore
}

func (m *errStore) GetLastSequenceNumber(_ context.Context, _ string) (int64, error) {
	return 0, assert.AnError
}
