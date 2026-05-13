//go:build integration

package membership

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

type fakeRing struct {
	mu    sync.Mutex
	addrs []string
	calls int
}

func (r *fakeRing) Set(addrs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs = append([]string(nil), addrs...)
	r.calls++
}

func (r *fakeRing) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.addrs))
	copy(out, r.addrs)
	return out
}

func (r *fakeRing) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()
	c, err := tcredis.Run(ctx, "redis:7-alpine",
		testcontainers.WithCmdArgs("--notify-keyspace-events", "K$gx"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	addr, err := c.ConnectionString(ctx)
	require.NoError(t, err)

	opt, err := redis.ParseURL(addr)
	require.NoError(t, err)
	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestWatcher_InitialScan(t *testing.T) {
	client := newRedisClient(t)
	ctx := t.Context()

	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-1:8081", "wss-1:8081", time.Minute).Err())
	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-2:8081", "wss-2:8081", time.Minute).Err())

	ring := &fakeRing{}
	w := NewWatcher(client, testKeyPrefix, ring)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	require.Eventually(t, func() bool {
		snap := ring.Snapshot()
		return len(snap) == 2
	}, 2*time.Second, 20*time.Millisecond, "초기 SCAN으로 ring에 두 멤버 반영")

	snap := ring.Snapshot()
	assert.ElementsMatch(t, []string{"wss-1:8081", "wss-2:8081"}, snap)
}

func TestWatcher_KeyspaceNotificationOnAdd(t *testing.T) {
	client := newRedisClient(t)
	ctx := t.Context()

	ring := &fakeRing{}
	w := NewWatcher(client, testKeyPrefix, ring)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	require.Eventually(t, func() bool {
		return ring.Calls() > 0
	}, time.Second, 20*time.Millisecond, "초기 reconcile 완료")

	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-new:8081", "wss-new:8081", time.Minute).Err())

	require.Eventually(t, func() bool {
		snap := ring.Snapshot()
		for _, a := range snap {
			if a == "wss-new:8081" {
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "keyspace notification으로 새 멤버 반영")
}

func TestWatcher_KeyspaceNotificationOnExpire(t *testing.T) {
	client := newRedisClient(t)
	ctx := t.Context()

	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-stable:8081", "wss-stable:8081", time.Minute).Err())
	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-tmp:8081", "wss-tmp:8081", 500*time.Millisecond).Err())

	ring := &fakeRing{}
	w := NewWatcher(client, testKeyPrefix, ring)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	require.Eventually(t, func() bool {
		snap := ring.Snapshot()
		return len(snap) == 2
	}, time.Second, 20*time.Millisecond, "초기 reconcile로 두 멤버 반영")

	require.Eventually(t, func() bool {
		snap := ring.Snapshot()
		return len(snap) == 1 && snap[0] == "wss-stable:8081"
	}, 3*time.Second, 50*time.Millisecond, "expired 이벤트 또는 다음 reconcile에서 wss-tmp 제거")
}

func TestWatcher_ForceReconcile(t *testing.T) {
	client := newRedisClient(t)
	ctx := t.Context()

	ring := &fakeRing{}
	w := NewWatcher(client, testKeyPrefix, ring)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	require.Eventually(t, func() bool { return ring.Calls() > 0 }, time.Second, 20*time.Millisecond)

	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-x:8081", "wss-x:8081", time.Minute).Err())
	w.ForceReconcile()

	require.Eventually(t, func() bool {
		for _, a := range ring.Snapshot() {
			if a == "wss-x:8081" {
				return true
			}
		}
		return false
	}, 500*time.Millisecond, 10*time.Millisecond, "ForceReconcile이 즉시 SCAN 트리거")
}

func TestWatcher_CrossInstance(t *testing.T) {
	client := newRedisClient(t)
	ctx := t.Context()

	ring1 := &fakeRing{}
	ring2 := &fakeRing{}
	w1 := NewWatcher(client, testKeyPrefix, ring1)
	w2 := NewWatcher(client, testKeyPrefix, ring2)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w1.Run(runCtx) }()
	go func() { _ = w2.Run(runCtx) }()

	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-shared:8081", "wss-shared:8081", time.Minute).Err())

	require.Eventually(t, func() bool {
		s1 := ring1.Snapshot()
		s2 := ring2.Snapshot()
		return contains(s1, "wss-shared:8081") && contains(s2, "wss-shared:8081")
	}, 2*time.Second, 20*time.Millisecond, "두 Watcher가 같은 Redis에서 동일 멤버 관찰")
}

func TestWatcher_EmptyMembersKeepsExistingRing(t *testing.T) {
	client := newRedisClient(t)
	ctx := t.Context()

	require.NoError(t, client.Set(ctx, testKeyPrefix+"wss-temp:8081", "wss-temp:8081", time.Minute).Err())

	ring := &fakeRing{}
	w := NewWatcher(client, testKeyPrefix, ring)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	require.Eventually(t, func() bool {
		return contains(ring.Snapshot(), "wss-temp:8081")
	}, 2*time.Second, 20*time.Millisecond, "초기 reconcile로 멤버 1개 반영")
	require.True(t, w.HasObservedMembers())

	require.NoError(t, client.Del(ctx, testKeyPrefix+"wss-temp:8081").Err())
	w.ForceReconcile()

	require.Eventually(t, func() bool {
		return !w.HasObservedMembers()
	}, 2*time.Second, 20*time.Millisecond, "Redis가 비어 HasObservedMembers는 false")

	assert.True(t, contains(ring.Snapshot(), "wss-temp:8081"),
		"empty 결과는 기존 ring을 유지해야 함")
}

func TestWatcher_ForceReconcileNonBlocking(t *testing.T) {
	t.Parallel()

	w := NewWatcher(nil, testKeyPrefix, &fakeRing{})

	done := make(chan struct{})
	go func() {
		for range 100 {
			w.ForceReconcile()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ForceReconcile이 block됨")
	}
}

func contains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
