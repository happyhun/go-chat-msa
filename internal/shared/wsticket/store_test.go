package wsticket

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, mr *miniredis.Miniredis) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestStore_Lifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, mr *miniredis.Miniredis, store *Store)
	}{
		{
			name: "Success: 발급한 티켓은 한 번만 소비된다",
			run: func(t *testing.T, _ *miniredis.Miniredis, store *Store) {
				ctx := t.Context()

				ticket, err := store.Issue(ctx, "user-123", 30*time.Second)
				require.NoError(t, err)
				assert.NoError(t, uuid.Validate(ticket))

				userID, ok, err := store.Consume(ctx, ticket)
				require.NoError(t, err)
				assert.True(t, ok)
				assert.Equal(t, "user-123", userID)

				_, ok, err = store.Consume(ctx, ticket)
				require.NoError(t, err)
				assert.False(t, ok)
			},
		},
		{
			name: "Failure: TTL이 지나면 소비할 수 없다",
			run: func(t *testing.T, mr *miniredis.Miniredis, store *Store) {
				ctx := t.Context()

				ticket, err := store.Issue(ctx, "user-456", 30*time.Second)
				require.NoError(t, err)
				mr.FastForward(31 * time.Second)

				_, ok, err := store.Consume(ctx, ticket)
				require.NoError(t, err)
				assert.False(t, ok)
			},
		},
		{
			name: "Success: 다른 인스턴스가 발급한 티켓을 같은 Redis로 소비한다",
			run: func(t *testing.T, mr *miniredis.Miniredis, store *Store) {
				ctx := t.Context()
				other := NewStore(newTestClient(t, mr))

				ticket, err := store.Issue(ctx, "user-1", 30*time.Second)
				require.NoError(t, err)

				userID, ok, err := other.Consume(ctx, ticket)
				require.NoError(t, err)
				assert.True(t, ok)
				assert.Equal(t, "user-1", userID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mr := miniredis.RunT(t)
			tt.run(t, mr, NewStore(newTestClient(t, mr)))
		})
	}
}
