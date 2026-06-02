package roomseq

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestStoreSetMaxAndGet(t *testing.T) {
	t.Parallel()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := t.Context()
	store := NewStore(client, "wss:room:seqfloor:")

	seq, err := store.Get(ctx, "room-1")
	require.NoError(t, err)
	require.Zero(t, seq)

	require.NoError(t, store.SetMax(ctx, "room-1", 10))
	seq, err = store.Get(ctx, "room-1")
	require.NoError(t, err)
	require.Equal(t, int64(10), seq)

	require.NoError(t, store.SetMax(ctx, "room-1", 7))
	seq, err = store.Get(ctx, "room-1")
	require.NoError(t, err)
	require.Equal(t, int64(10), seq)

	require.NoError(t, store.SetMax(ctx, "room-1", 13))
	seq, err = store.Get(ctx, "room-1")
	require.NoError(t, err)
	require.Equal(t, int64(13), seq)
}
