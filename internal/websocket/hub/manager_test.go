package hub

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/websocket/roomlease"
	"go-chat-msa/internal/websocket/roomseq"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

type staticSequenceFloorStore struct {
	seq int64
}

func (s staticSequenceFloorStore) Get(_ context.Context, _ string) (int64, error) {
	return s.seq, nil
}

func (s staticSequenceFloorStore) SetMax(_ context.Context, _ string, _ int64) error {
	return nil
}

type recordingSequenceFloorStore struct {
	seq    int64
	setSeq atomic.Int64
}

func (s *recordingSequenceFloorStore) Get(_ context.Context, _ string) (int64, error) {
	return s.seq, nil
}

func (s *recordingSequenceFloorStore) SetMax(_ context.Context, _ string, seq int64) error {
	s.setSeq.Store(seq)
	return nil
}

type errorSequenceFloorStore struct{}

func (s errorSequenceFloorStore) Get(_ context.Context, _ string) (int64, error) {
	return 0, assert.AnError
}

func (s errorSequenceFloorStore) SetMax(_ context.Context, _ string, _ int64) error {
	return nil
}

func TestManager_NewManager(t *testing.T) {
	t.Parallel()

	t.Run("Success: 매니저 인스턴스 정상 생성", func(t *testing.T) {
		t.Parallel()
		manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil)
		assert.NotNil(t, manager)
	})
}

func TestManager_PrepareRegisterReleasesLeaseWhenSequenceInitializationFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		store MessageStore
		floor SequenceFloorStore
	}{
		{
			name:  "DB sequence lookup fails",
			store: &errStore{},
		},
		{
			name:  "Redis sequence floor lookup fails",
			store: &mockStore{lastSeq: 10},
			floor: errorSequenceFloorStore{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })

			leaseStore := roomlease.NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second)
			manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, tt.store, 100*time.Millisecond)
			manager.SetRoomLeaseStore(leaseStore)
			if tt.floor != nil {
				manager.SetSequenceFloorStore(tt.floor)
			}

			runCtx, stop := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				manager.Run(runCtx)
			}()
			t.Cleanup(func() {
				stop()
				<-done
			})

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			registration, err := manager.PrepareRegister(ctx, "room-1")

			require.Nil(t, registration)
			require.ErrorIs(t, err, ErrRoomSequenceUnavailable)
			require.False(t, srv.Exists("wss:room:lease:room-1"))
			require.Empty(t, manager.snapshotLeases())
		})
	}
}

func TestHub_Functional(t *testing.T) {
	t.Parallel()

	t.Run("Success: 다수 세션의 등록 및 메시지 브로드캐스트", func(t *testing.T) {
		t.Parallel()
		h := newHub("room1", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
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
		h := newHub("conflict-room", testSessionConfig(), 5*time.Minute, nil, nil, time.Second, nil)
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

		closed, err := manager.ForceCloseRoom(t.Context(), "none")
		assert.NoError(t, err)
		assert.False(t, closed, "존재하지 않는 방은 close되지 않음")
	})

	t.Run("Success: 활성화된 방 강제 종료", func(t *testing.T) {
		t.Parallel()
		manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go manager.Run(ctx)

		_ = manager.Broadcast(t.Context(), &Message{RoomID: "r2", SenderID: "sys", Type: "system"})
		time.Sleep(50 * time.Millisecond)
		closed, err := manager.ForceCloseRoom(t.Context(), "r2")
		assert.NoError(t, err)
		assert.True(t, closed, "활성 방은 close됨")
	})
}

func TestManager_ShutdownStopsTimedOutHubsBeforeClosingPersistenceQueue(t *testing.T) {
	t.Parallel()

	store := &retryOnlyStore{}
	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, store, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()

	err := manager.Broadcast(t.Context(), &Message{
		RoomID:   "retry-room",
		SenderID: "user-1",
		Content:  "hello",
		Type:     "chat",
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return store.saveCalls.Load() > 0
	}, time.Second, 10*time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("manager should stop even when hub persistence drain times out")
	}
}

func TestManager_PrepareRegisterCancelReleasesNewLease(t *testing.T) {
	t.Parallel()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil, 100*time.Millisecond)
	manager.SetRoomLeaseStore(roomlease.NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go manager.Run(ctx)

	registration, err := manager.PrepareRegister(t.Context(), "room-1")
	require.NoError(t, err)
	require.NotNil(t, registration)

	registration.Cancel()

	require.Eventually(t, func() bool {
		return !srv.Exists("wss:room:lease:room-1")
	}, time.Second, 10*time.Millisecond)
}

func TestManager_ShutdownHubsClosesSessionsWithHandoffCode(t *testing.T) {
	t.Parallel()

	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil, 100*time.Millisecond)
	h := newHub("room-1", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.run(ctx)

	clientConn := registerTestSession(t, h, "user-1")
	defer clientConn.Close()

	manager.shutdownHubs(t.Context(), map[string]*Hub{h.roomID: h})

	clientConn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err := clientConn.ReadMessage()
	var closeErr *websocket.CloseError
	require.True(t, errors.As(err, &closeErr), "expected websocket close error, got %v", err)
	require.Equal(t, handoffCloseCode, closeErr.Code)
	require.Equal(t, handoffCloseReason, closeErr.Text)
}

func TestManager_ReleaseLeaseDefersUntilDrainComplete(t *testing.T) {
	t.Parallel()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	leaseStore := roomlease.NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second)
	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil, 100*time.Millisecond)
	manager.SetRoomLeaseStore(leaseStore)

	lease, err := leaseStore.Acquire(t.Context(), "room-1")
	require.NoError(t, err)
	manager.trackLease(lease)

	h := newHub("room-1", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
	h.pendingPersist.Store(1)

	manager.releaseLease(t.Context(), "room-1", h)

	require.True(t, srv.Exists("wss:room:lease:room-1"))
	require.Len(t, manager.snapshotLeases(), 1)

	manager.releaseAllLeases(t.Context(), map[string]*Hub{})

	require.True(t, srv.Exists("wss:room:lease:room-1"))
	require.Len(t, manager.snapshotLeases(), 1)

	h.pendingPersist.Store(0)
	h.notifyPersistDone()

	require.Eventually(t, func() bool {
		return !srv.Exists("wss:room:lease:room-1") && len(manager.snapshotLeases()) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestManager_ReleaseLeaseRecordsSequenceFloorWhenPersistFailed(t *testing.T) {
	t.Parallel()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	leaseStore := roomlease.NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second)
	floorStore := roomseq.NewStore(client, "wss:room:seqfloor:")
	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil, 100*time.Millisecond)
	manager.SetRoomLeaseStore(leaseStore)
	manager.SetSequenceFloorStore(floorStore)

	lease, err := leaseStore.Acquire(t.Context(), "room-1")
	require.NoError(t, err)
	manager.trackLease(lease)

	h := newHub("room-1", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)
	h.lastSequence.Store(43)
	h.persistFailed.Store(true)

	manager.releaseLease(t.Context(), "room-1", h)

	require.False(t, srv.Exists("wss:room:lease:room-1"))
	require.Empty(t, manager.snapshotLeases())

	manager.releaseAllLeases(t.Context(), map[string]*Hub{})

	require.False(t, srv.Exists("wss:room:lease:room-1"))
	require.Empty(t, manager.snapshotLeases())

	floor, err := floorStore.Get(t.Context(), "room-1")
	require.NoError(t, err)
	require.Equal(t, int64(43), floor)
}

func TestManager_ReleaseLeaseDeletesWhenDrainComplete(t *testing.T) {
	t.Parallel()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	leaseStore := roomlease.NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second)
	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil, 100*time.Millisecond)
	manager.SetRoomLeaseStore(leaseStore)

	lease, err := leaseStore.Acquire(t.Context(), "room-1")
	require.NoError(t, err)
	manager.trackLease(lease)

	h := newHub("room-1", testSessionConfig(), time.Minute, nil, nil, time.Second, nil)

	manager.releaseLease(t.Context(), "room-1", h)

	require.False(t, srv.Exists("wss:room:lease:room-1"))
	require.Empty(t, manager.snapshotLeases())
}

func TestSequenceFloorMessageStoreGetLastSequenceNumberUsesFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dbSeq   int64
		floor   int64
		wantSeq int64
	}{
		{
			name:    "floor is higher than DB",
			dbSeq:   10,
			floor:   13,
			wantSeq: 13,
		},
		{
			name:    "DB is higher than floor",
			dbSeq:   20,
			floor:   13,
			wantSeq: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &sequenceFloorMessageStore{
				base:  &mockStore{lastSeq: tt.dbSeq},
				floor: staticSequenceFloorStore{seq: tt.floor},
			}

			seq, err := store.GetLastSequenceNumber(t.Context(), "room-1")
			require.NoError(t, err)
			require.Equal(t, tt.wantSeq, seq)
		})
	}
}

func TestManager_ReleaseManagedLeaseKeepsLeaseOnRedisError(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  10 * time.Millisecond,
		ReadTimeout:  10 * time.Millisecond,
		WriteTimeout: 10 * time.Millisecond,
		MaxRetries:   0,
	})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil, 100*time.Millisecond)
	manager.SetRoomLeaseStore(roomlease.NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second))
	lease := &roomlease.Lease{
		RoomID:    "room-1",
		OwnerAddr: "10.0.0.1:8081",
		Token:     "token-1",
	}
	manager.trackLease(lease)

	manager.leasesMu.RLock()
	managed := manager.leases["room-1"]
	manager.leasesMu.RUnlock()

	released := manager.releaseManagedLease(t.Context(), "room-1", managed, hubLeaseState{})

	require.False(t, released)
	require.Len(t, manager.snapshotLeases(), 1)
}

func TestManager_ReleaseLeaseAfterPersistFailureKeepsLeaseWhenReleaseFails(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  10 * time.Millisecond,
		ReadTimeout:  10 * time.Millisecond,
		WriteTimeout: 10 * time.Millisecond,
		MaxRetries:   0,
	})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	floorStore := &recordingSequenceFloorStore{}
	manager := NewManager(testManagerConfig(), config.RateLimitConfig{}, nil, 100*time.Millisecond)
	manager.SetRoomLeaseStore(roomlease.NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second))
	manager.SetSequenceFloorStore(floorStore)
	lease := &roomlease.Lease{
		RoomID:    "room-1",
		OwnerAddr: "10.0.0.1:8081",
		Token:     "token-1",
	}
	manager.trackLease(lease)

	manager.leasesMu.RLock()
	managed := manager.leases["room-1"]
	manager.leasesMu.RUnlock()

	released := manager.releaseLeaseAfterPersistFailure(t.Context(), "room-1", managed, hubLeaseState{
		lastSequence:  43,
		persistFailed: true,
	})

	require.False(t, released)
	require.Equal(t, int64(43), floorStore.setSeq.Load())
	require.Len(t, manager.snapshotLeases(), 1)
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

type retryOnlyStore struct {
	mockStore
	saveCalls atomic.Int64
}

func (s *retryOnlyStore) SaveMany(_ context.Context, _ []*Message) error {
	s.saveCalls.Add(1)
	return status.Error(codes.Unavailable, "temporary outage")
}
