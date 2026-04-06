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

func (m *mockStore) SaveMany(_ context.Context, msgs []*Message) {
	m.persisted = append(m.persisted, msgs...)
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
		h := newHub(roomID, testSessionConfig(), 5*time.Minute, store, nil, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
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
		name     string
		store    MessageStore
		expected int64
	}{
		{
			name:     "Success: DB에서 마지막 시퀀스 번호 정상 로드",
			store:    &mockStore{lastSeq: 50},
			expected: 50,
		},
		{
			name:     "Failure: DB 에러 발생 시 시퀀스 번호 0으로 초기화",
			store:    &errStore{},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHub("room1", testSessionConfig(), time.Minute, tt.store, nil, nil)
			h.initializeSequence(t.Context())
			assert.Equal(t, tt.expected, h.lastSequence.Load())
		})
	}
}

func TestHub_Lifecycle(t *testing.T) {
	t.Parallel()

	t.Run("Failure: 종료된 Hub에 세션 등록 시도 시 에러 반환", func(t *testing.T) {
		t.Parallel()
		h := newHub("stop-room", testSessionConfig(), time.Minute, nil, nil, nil)
		close(h.doneCh)
		err := h.register(t.Context(), nil, "user1")
		require.Error(t, err)
		assert.ErrorContains(t, err, "hub closed")
	})

	t.Run("Success: 종료된 Hub에 브로드캐스트 시 패닉 방지", func(t *testing.T) {
		t.Parallel()
		h := newHub("stop-room", testSessionConfig(), time.Minute, nil, nil, nil)
		close(h.doneCh)
		h.broadcast(t.Context(), &Message{RoomID: "stop-room"})
	})

	t.Run("Success: Shutdown 호출 시 모든 세션 정리", func(t *testing.T) {
		t.Parallel()
		h := newHub("shutdown-room", testSessionConfig(), time.Minute, nil, nil, nil)
		go func() {
			serverConn, _ := createTestWSPair(t)
			_ = h.register(t.Context(), serverConn, "user1")
		}()

		time.Sleep(50 * time.Millisecond)
		h.shutdown()
		assert.Empty(t, h.sessions)
	})
}

type errStore struct {
	mockStore
}

func (m *errStore) GetLastSequenceNumber(_ context.Context, _ string) (int64, error) {
	return 0, assert.AnError
}
