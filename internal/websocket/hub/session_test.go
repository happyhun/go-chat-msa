package hub

import (
	"context"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSession_ReadPump(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		payload       any
		isRaw         bool
		expectMessage bool
	}{
		{
			name: "Success: 유효한 채팅 메시지 수신 및 브로드캐스트 전달",
			payload: map[string]string{
				"content":       "Hello World",
				"client_msg_id": "client-1",
				"type":          "chat",
			},
			expectMessage: true,
		},
		{
			name:          "Failure: 잘못된 JSON 형식 메시지 수신",
			payload:       "{invalid-json",
			isRaw:         true,
			expectMessage: false,
		},
		{
			name: "Failure: 필수 필드(content) 누락 메시지 무시",
			payload: map[string]string{
				"client_msg_id": "client-2",
			},
			expectMessage: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			serverConn, clientConn := createTestWSPair(t)
			t.Cleanup(func() { clientConn.Close() })

			unregisterCh := make(chan *session, 1)
			broadcastCh := make(chan *Message, 1)

			cfg := sessionConfig{
				writeWait:  10 * time.Second,
				pongWait:   60 * time.Second,
				pingPeriod: 54 * time.Second,
			}

			s := newSession(cfg, serverConn, "user1", "room1", unregisterCh, broadcastCh, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			go s.readPump(ctx)

			var err error
			if tt.isRaw {
				err = clientConn.WriteMessage(websocket.TextMessage, []byte(tt.payload.(string)))
			} else {
				err = clientConn.WriteJSON(tt.payload)
			}
			require.NoError(t, err)

			if tt.expectMessage {
				select {
				case msg := <-broadcastCh:
					assert.NotEmpty(t, msg.Content)
				case <-time.After(500 * time.Millisecond):
					t.Fatal("timeout waiting for broadcast message")
				}
			} else {
				select {
				case <-broadcastCh:
					t.Fatal("message should not have been broadcasted")
				case <-time.After(100 * time.Millisecond):

				}
			}
		})
	}

}

func TestSession_ReadPump_Unregister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
	}{
		{
			name: "Success: 클라이언트 연결 종료 시 Unregister 트리거",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			serverConn, clientConn := createTestWSPair(t)
			unregisterCh := make(chan *session, 1)
			cfg := sessionConfig{
				writeWait:  10 * time.Second,
				pongWait:   60 * time.Second,
				pingPeriod: 54 * time.Second,
			}
			s := newSession(cfg, serverConn, "user1", "room1", unregisterCh, make(chan *Message, 1), nil)

			go s.run(t.Context())

			clientConn.Close()

			select {
			case session := <-unregisterCh:
				assert.Equal(t, s, session)
			case <-time.After(1 * time.Second):
				t.Fatal("unregister should be called after connection close")
			}
		})
	}
}

func TestSession_WritePumpAndSend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, s *session, clientConn *websocket.Conn)
	}{
		{
			name: "Success: 메시지 전송 및 클라이언트 수신 확인",
			run: func(t *testing.T, s *session, clientConn *websocket.Conn) {
				s.send(context.Background(), []byte(`{"test":true}`))

				clientConn.SetReadDeadline(time.Now().Add(1 * time.Second))
				msgType, data, err := clientConn.ReadMessage()
				require.NoError(t, err)
				assert.Equal(t, websocket.TextMessage, msgType)
				assert.JSONEq(t, `{"test":true}`, string(data))
			},
		},
		{
			name: "Success: 정기적인 Ping 메시지 전송 확인",
			run: func(t *testing.T, s *session, clientConn *websocket.Conn) {
				clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				var receivedPing bool
				clientConn.SetPingHandler(func(appData string) error {
					receivedPing = true
					return nil
				})

				clientConn.ReadMessage()
				assert.True(t, receivedPing, "ping message should be received")
			},
		},
		{
			name: "Success: 버퍼 초과 상황에서의 세션 정상 종료",
			run: func(t *testing.T, s *session, clientConn *websocket.Conn) {
				for range sendBufferSize + 10 {
					s.send(context.Background(), []byte(`"spam"`))
				}

				s.close()

				deadline := time.Now().Add(3 * time.Second)
				var err error
				for time.Now().Before(deadline) {
					clientConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
					_, _, err = clientConn.ReadMessage()
					if err != nil {
						break
					}
				}
				assert.Error(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			serverConn, clientConn := createTestWSPair(t)
			defer clientConn.Close()

			cfg := sessionConfig{
				writeWait:  10 * time.Second,
				pongWait:   60 * time.Second,
				pingPeriod: 50 * time.Millisecond,
			}

			s := newSession(cfg, serverConn, "user2", "room2", nil, nil, nil)
			go s.writePump(context.Background())

			tt.run(t, s, clientConn)
		})
	}
}

func TestSession_Run(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, s *session, clientConn *websocket.Conn, cancel context.CancelFunc, done <-chan struct{})
	}{
		{
			name: "Success: Run 루프 활성화 중 메시지 전송",
			run: func(t *testing.T, s *session, clientConn *websocket.Conn, cancel context.CancelFunc, done <-chan struct{}) {
				s.send(context.Background(), []byte("hello"))
				_, data, err := clientConn.ReadMessage()
				require.NoError(t, err)
				assert.Equal(t, "hello", string(data))
			},
		},
		{
			name: "Success: 컨텍스트 취소 시 세션 리소스 정리",
			run: func(t *testing.T, s *session, clientConn *websocket.Conn, cancel context.CancelFunc, done <-chan struct{}) {
				cancel()

				select {
				case <-done:
				case <-time.After(1 * time.Second):
					t.Fatal("session.run should return after context cancel")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			serverConn, clientConn := createTestWSPair(t)
			defer clientConn.Close()

			cfg := sessionConfig{
				writeWait:  10 * time.Second,
				pongWait:   60 * time.Second,
				pingPeriod: 54 * time.Second,
			}

			unregisterCh := make(chan *session, 1)
			s := newSession(cfg, serverConn, "u3", "r3", unregisterCh, make(chan *Message, 10), nil)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan struct{})
			go func() {
				s.run(ctx)
				close(done)
			}()
			tt.run(t, s, clientConn, cancel, done)
		})
	}
}
