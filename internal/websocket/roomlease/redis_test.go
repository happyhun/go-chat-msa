package roomlease

import (
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestStoreAcquireRenewRelease(t *testing.T) {
	t.Parallel()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := t.Context()
	storeA := NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second)
	storeB := NewStore(client, "wss:room:lease:", "10.0.0.2:8081", 30*time.Second)

	leaseA, err := storeA.Acquire(ctx, "room-1")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1:8081", leaseA.OwnerAddr)

	_, err = storeB.Acquire(ctx, "room-1")
	require.ErrorIs(t, err, ErrBusy)

	results := storeA.RenewBatch(ctx, []*Lease{leaseA})
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)

	require.NoError(t, storeA.Release(ctx, leaseA))

	leaseB, err := storeB.Acquire(ctx, "room-1")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.2:8081", leaseB.OwnerAddr)
}

func TestStoreTokenMismatch(t *testing.T) {
	t.Parallel()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := t.Context()
	store := NewStore(client, "wss:room:lease:", "10.0.0.1:8081", 30*time.Second)

	lease, err := store.Acquire(ctx, "room-1")
	require.NoError(t, err)

	other := *lease
	other.Token = "other-token"

	results := store.RenewBatch(ctx, []*Lease{&other})
	require.Len(t, results, 1)
	require.ErrorIs(t, results[0].Err, ErrLost)

	err = store.Release(ctx, &other)
	require.True(t, errors.Is(err, ErrLost))

	require.NoError(t, store.Release(ctx, lease))
}
