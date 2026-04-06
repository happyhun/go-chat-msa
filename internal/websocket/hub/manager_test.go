package hub

import (
	"context"
	"encoding/json"
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
		MaxLength:   10000,
	}
}

func TestManager_NewManager(t *testing.T) {
	t.Parallel()

	t.Run("Success: 매니저 인스턴스 정상 생성", func(t *testing.T) {
		t.Parallel()
		manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil)
		assert.NotNil(t, manager)
	})
}

func TestHub_Functional(t *testing.T) {
	t.Parallel()

	t.Run("Success: 다수 세션의 등록 및 메시지 브로드캐스트", func(t *testing.T) {
		t.Parallel()
		h := newHub("room1", testSessionConfig(), time.Minute, nil, nil, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go h.run(ctx)

		client1 := registerTestSession(t, h, "user1")
		client2 := registerTestSession(t, h, "user2")

		msg := &Message{
			ID:       "msg1",
			RoomID:   "test-room",
			SenderID: "user1",
			Content:  "hello world",
			Type:     "chat",
		}
		h.broadcast(ctx, msg)

		for _, cc := range []*websocket.Conn{client1, client2} {
			cc.SetReadDeadline(time.Now().Add(1 * time.Second))
			_, data, err := cc.ReadMessage()
			require.NoError(t, err)
			var received Message
			err = json.Unmarshal(data, &received)
			assert.NoError(t, err)
			assert.Equal(t, "hello world", received.Content)
		}
	})

	t.Run("Success: 동일 유저 중복 등록 시 이전 세션 강제 종료(Conflict)", func(t *testing.T) {
		t.Parallel()
		h := newHub("conflict-room", testSessionConfig(), 5*time.Minute, nil, nil, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go h.run(ctx)

		client1 := registerTestSession(t, h, "user1")
		_ = registerTestSession(t, h, "user1")

		client1.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, data, err := client1.ReadMessage()
		require.NoError(t, err)

		var msg Message
		err = json.Unmarshal(data, &msg)
		require.NoError(t, err)
		assert.Equal(t, "conflict", msg.Type, "previous session should receive conflict message")

		_, _, err = client1.ReadMessage()
		assert.Error(t, err, "previous session connection should be closed")
	})
}

func TestManager_Broadcast(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, m *Manager)
		expectMsg bool
	}{
		{
			name:      "Success: 존재하지 않는 방에 브로드캐스트 시 무시",
			setup:     func(t *testing.T, m *Manager) {},
			expectMsg: false,
		},
		{
			name: "Success: 존재하는 방에 시스템 메시지 브로드캐스트",
			setup: func(t *testing.T, m *Manager) {
				serverConn, clientConn := createTestWSPair(t)
				t.Cleanup(func() { clientConn.Close() })
				err := m.Register(t.Context(), serverConn, "sys-user", "room-1")
				require.NoError(t, err)
			},
			expectMsg: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go manager.Run(ctx)

			tt.setup(t, manager)

			msg := &Message{
				ID:       "sys-1",
				RoomID:   "room-1",
				SenderID: "system",
				Content:  "hello",
				Type:     "system",
			}
			err := manager.Broadcast(t.Context(), msg)
			assert.NoError(t, err)
		})
	}
}

func TestManager_Register(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(cancelFunc context.CancelFunc)
		useCanceled bool
		expectedErr string
	}{
		{
			name: "Success: 정상 세션 등록",
		},
		{
			name:        "Failure: 컨텍스트가 이미 취소된 상태에서의 등록 시도",
			useCanceled: true,
		},
		{
			name: "Failure: 매니저가 중단된 상태에서의 등록 시도",
			setup: func(cancel context.CancelFunc) {
				cancel()
				time.Sleep(10 * time.Millisecond)
			},
			expectedErr: "manager stopped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go manager.Run(ctx)

			if tt.setup != nil {
				tt.setup(cancel)
			}

			regCtx := t.Context()
			if tt.useCanceled {
				c, cnl := context.WithCancel(t.Context())
				cnl()
				regCtx = c
			}

			serverConn, clientConn := createTestWSPair(t)
			t.Cleanup(func() { clientConn.Close() })

			err := manager.Register(regCtx, serverConn, "u1", "r1")
			if tt.expectedErr != "" {
				assert.ErrorContains(t, err, tt.expectedErr)
			} else if tt.useCanceled {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestManager_ForceCloseRoom(t *testing.T) {
	t.Parallel()

	t.Run("Success: 존재하지 않는 방 강제 종료 시도 (정상 처리)", func(t *testing.T) {
		t.Parallel()
		manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go manager.Run(ctx)

		err := manager.ForceCloseRoom(t.Context(), "none")
		assert.NoError(t, err)
	})

	t.Run("Success: 활성화된 방 강제 종료", func(t *testing.T) {
		t.Parallel()
		manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go manager.Run(ctx)

		_ = manager.Broadcast(t.Context(), &Message{RoomID: "r2", SenderID: "sys", Type: "system"})
		time.Sleep(50 * time.Millisecond)
		err := manager.ForceCloseRoom(t.Context(), "r2")
		assert.NoError(t, err)
	})
}

func TestNewSystemMessage(t *testing.T) {
	t.Parallel()

	t.Run("Success: 시스템 메시지 구조체 정상 생성", func(t *testing.T) {
		t.Parallel()
		msg, err := NewSystemMessage("test-room", "hello")
		assert.NoError(t, err)
		assert.Equal(t, "test-room", msg.RoomID)
		assert.Equal(t, "hello", msg.Content)
		assert.Equal(t, "system", msg.Type)
		assert.Equal(t, systemSenderID, msg.SenderID)
	})
}
