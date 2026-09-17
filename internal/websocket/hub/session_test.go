package hub

import (
	"context"
	"encoding/json"
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

func TestSession_FrameNumbering(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := createTestWSPair(t)
	defer func() { _ = clientConn.Close() }()

	s := newTestSession(serverConn, "user2", "room2", nil)
	go s.writePump(context.Background())

	for want := 1; want <= 3; want++ {
		s.send(context.Background(), egressPacket{data: []byte(`{"content":"hi"}`)})

		require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(time.Second)))
		_, data, err := clientConn.ReadMessage()
		require.NoError(t, err)

		var frame struct {
			Content string `json:"content"`
			FrameNo int64  `json:"frame_no"`
		}
		require.NoError(t, json.Unmarshal(data, &frame))
		assert.Equal(t, "hi", frame.Content)
		assert.Equal(t, int64(want), frame.FrameNo, "frame_no must increase by one per frame")
	}
}

func TestSession_WriteFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		frameNo int64
		want    string
	}{
		{
			name:    "Success: 객체 끝에 frame_no 추가",
			payload: `{"id":"m1","content":"hi"}`,
			frameNo: 7,
			want:    `{"id":"m1","content":"hi","frame_no":7}`,
		},
		{
			name:    "Success: 중괄호가 없으면 원본 유지",
			payload: `not-json`,
			frameNo: 3,
			want:    `not-json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			serverConn, clientConn := createTestWSPair(t)
			defer func() { _ = clientConn.Close() }()

			payload := []byte(tt.payload)
			s := newTestSession(serverConn, "user", "room", nil)
			require.NoError(t, s.writeFrame(egressPacket{data: payload, frameNo: tt.frameNo}))

			require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(time.Second)))
			_, data, err := clientConn.ReadMessage()
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(data))
			assert.Equal(t, tt.payload, string(payload), "shared payload must not be modified")
		})
	}
}

func TestSession_SendDropsFrameWhenBufferFull(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := createTestWSPair(t)
	defer func() { _ = clientConn.Close() }()

	s := newTestSession(serverConn, "user3", "room3", nil)

	for range sendBufferSize + 10 {
		s.send(context.Background(), egressPacket{data: []byte(`{"content":"spam"}`)})
	}

	assert.False(t, s.isClosed(), "session must stay open when frames are dropped")
	assert.Len(t, s.sendCh, sendBufferSize)
	assert.Equal(t, int64(sendBufferSize+10), s.frameNo, "dropped frames still consume a frame number")
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
