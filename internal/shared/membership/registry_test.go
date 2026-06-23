package membership

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKeyPrefix = "wss:member:"

func newTestRegistry(t *testing.T, addr string, ttl, heartbeat time.Duration) (*Registry, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	reg := NewRegistry(client, testKeyPrefix, addr, ttl, heartbeat)
	return reg, client, mr
}

func TestRegistry_RegisterOnRun(t *testing.T) {
	t.Parallel()
	r, client, _ := newTestRegistry(t, "wss-1:8081", 30*time.Second, 10*time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		val, err := client.Get(t.Context(), testKeyPrefix+"wss-1:8081").Result()
		return err == nil && val == r.token
	}, time.Second, 10*time.Millisecond, "Run 시작 시 SET 호출되어야 함")
}

func TestRegistry_HeartbeatExtendsTTL(t *testing.T) {
	t.Parallel()
	r, client, mr := newTestRegistry(t, "wss-1:8081", 30*time.Second, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		return mr.Exists(testKeyPrefix + "wss-1:8081")
	}, time.Second, 10*time.Millisecond)

	mr.FastForward(20 * time.Second)
	time.Sleep(120 * time.Millisecond)
	mr.FastForward(20 * time.Second)

	val, err := client.Get(t.Context(), testKeyPrefix+"wss-1:8081").Result()
	require.NoError(t, err)
	assert.Equal(t, r.token, val, "heartbeat가 TTL을 갱신해 expire 안 됨")
}

func TestRegistry_DeregisterOnCancel(t *testing.T) {
	t.Parallel()
	r, client, _ := newTestRegistry(t, "wss-2:8081", 30*time.Second, 10*time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		_, err := client.Get(t.Context(), testKeyPrefix+"wss-2:8081").Result()
		return err == nil
	}, time.Second, 10*time.Millisecond)

	cancel()
	<-done

	_, err := client.Get(t.Context(), testKeyPrefix+"wss-2:8081").Result()
	assert.True(t, errors.Is(err, redis.Nil), "cancel 후 DEL 호출되어야 함")
}

func TestRegistry_HeartbeatReregistersOnMissingKey(t *testing.T) {
	t.Parallel()
	r, client, mr := newTestRegistry(t, "wss-3:8081", 30*time.Second, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		return mr.Exists(testKeyPrefix + "wss-3:8081")
	}, time.Second, 10*time.Millisecond, "초기 등록")

	mr.Del(testKeyPrefix + "wss-3:8081")
	require.False(t, mr.Exists(testKeyPrefix+"wss-3:8081"), "강제 삭제 직후")

	require.Eventually(t, func() bool {
		return mr.Exists(testKeyPrefix + "wss-3:8081")
	}, time.Second, 10*time.Millisecond, "다음 heartbeat에서 재등록")

	val, err := client.Get(t.Context(), testKeyPrefix+"wss-3:8081").Result()
	require.NoError(t, err)
	assert.Equal(t, r.token, val, "재등록된 값은 동일 token")
}

func TestRegistry_DeregisterIgnoresMismatchedToken(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	r1 := NewRegistry(client, testKeyPrefix, "wss-x:8081", 30*time.Second, 10*time.Second)

	require.NoError(t, client.Set(t.Context(), testKeyPrefix+"wss-x:8081", "other-token", 30*time.Second).Err())

	r1.deregister()

	val, err := client.Get(t.Context(), testKeyPrefix+"wss-x:8081").Result()
	require.NoError(t, err)
	assert.Equal(t, "other-token", val, "token이 다르면 삭제되지 않음")
}

func TestRegistry_DeregisterDeletesMatchingToken(t *testing.T) {
	t.Parallel()
	r, client, _ := newTestRegistry(t, "wss-y:8081", 30*time.Second, 10*time.Second)

	require.NoError(t, client.Set(t.Context(), testKeyPrefix+"wss-y:8081", r.token, 30*time.Second).Err())

	r.deregister()

	_, err := client.Get(t.Context(), testKeyPrefix+"wss-y:8081").Result()
	assert.True(t, errors.Is(err, redis.Nil), "token이 같으면 삭제됨")
}
