package wsgateway

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestTicketStore(t *testing.T) (*TicketStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewTicketStore(client), mr
}

func TestTicketStore_Lifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "Success: 티켓 저장 및 1회성 사용 검증",
			run: func(t *testing.T) {
				store, _ := newTestTicketStore(t)
				ctx := t.Context()

				ticket := "test-ticket"
				userID := "user-123"

				require.NoError(t, store.Set(ctx, ticket, userID, time.Minute))

				storedUserID, ok, err := store.GetAndDelete(ctx, ticket)
				require.NoError(t, err)
				assert.True(t, ok)
				assert.Equal(t, userID, storedUserID)

				_, ok, err = store.GetAndDelete(ctx, ticket)
				require.NoError(t, err)
				assert.False(t, ok)
			},
		},
		{
			name: "Failure: TTL 만료 후 자동 삭제",
			run: func(t *testing.T) {
				store, mr := newTestTicketStore(t)
				ctx := t.Context()

				require.NoError(t, store.Set(ctx, "expiring", "user-456", 30*time.Second))
				mr.FastForward(31 * time.Second)

				_, ok, err := store.GetAndDelete(ctx, "expiring")
				require.NoError(t, err)
				assert.False(t, ok)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func TestTicketStore_CrossInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
	}{
		{
			name: "Success: 다른 인스턴스에서 발급한 티켓을 같은 Redis 통해 검증",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mr := miniredis.RunT(t)
			client1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			client2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() {
				_ = client1.Close()
				_ = client2.Close()
			})

			s1 := NewTicketStore(client1)
			s2 := NewTicketStore(client2)
			ctx := t.Context()

			require.NoError(t, s1.Set(ctx, "shared", "user-1", time.Minute))

			userID, ok, err := s2.GetAndDelete(ctx, "shared")
			require.NoError(t, err)
			assert.True(t, ok, "다른 인스턴스의 티켓을 검증할 수 있어야 함")
			assert.Equal(t, "user-1", userID)
		})
	}
}
