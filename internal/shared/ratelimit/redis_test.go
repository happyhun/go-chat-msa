package ratelimit

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRedisLimiter(t *testing.T, rate, burst int) *RedisLimiter {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb, rate, burst)
}

func TestRedisLimiter_Allow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rate     int
		burst    int
		requests int
		wantLast bool
	}{
		{
			name:     "Success: 버스트 이내 요청 허용",
			rate:     10,
			burst:    5,
			requests: 4,
			wantLast: true,
		},
		{
			name:     "Failure: 버스트 초과 시 요청 거부",
			rate:     10,
			burst:    5,
			requests: 6,
			wantLast: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limiter := newTestRedisLimiter(t, tt.rate, tt.burst)

			key := "user1"
			var last bool
			for range tt.requests {
				allowed, err := limiter.Allow(t.Context(), key)
				require.NoError(t, err)
				last = allowed
			}
			assert.Equal(t, tt.wantLast, last)
		})
	}
}

func TestRedisLimiter_KeyIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
	}{
		{
			name: "Success: 서로 다른 키는 독립적으로 제한",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limiter := newTestRedisLimiter(t, 10, 2)
			ctx := t.Context()

			_, err := limiter.Allow(ctx, "key-a")
			require.NoError(t, err)
			_, err = limiter.Allow(ctx, "key-a")
			require.NoError(t, err)
			a, err := limiter.Allow(ctx, "key-a")
			require.NoError(t, err)
			assert.False(t, a)

			b, err := limiter.Allow(ctx, "key-b")
			require.NoError(t, err)
			assert.True(t, b)
		})
	}
}

func TestRedisLimiter_CrossInstanceShared(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
	}{
		{
			name: "Success: 두 인스턴스가 같은 Redis 통해 글로벌 limit 공유",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mr := miniredis.RunT(t)
			rdb1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			rdb2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() {
				_ = rdb1.Close()
				_ = rdb2.Close()
			})

			l1 := NewRedis(rdb1, 10, 2)
			l2 := NewRedis(rdb2, 10, 2)
			ctx := t.Context()

			a1, err := l1.Allow(ctx, "shared")
			require.NoError(t, err)
			assert.True(t, a1, "첫 요청은 허용")

			a2, err := l2.Allow(ctx, "shared")
			require.NoError(t, err)
			assert.True(t, a2, "다른 인스턴스의 두 번째도 허용 (burst=2 안)")

			a3, err := l1.Allow(ctx, "shared")
			require.NoError(t, err)
			assert.False(t, a3, "burst 소진 → 거부 (글로벌 limit 공유 입증)")
		})
	}
}
