package ratelimit

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTokenBucketLimiter_Allow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rps        float64
		burst      int
		ttl        time.Duration
		requests   int
		wantLast   bool
		sleepAfter time.Duration
		wantAfter  bool
	}{
		{
			name:       "Success: 버스트 이내 요청 허용",
			rps:        10,
			burst:      5,
			ttl:        100 * time.Millisecond,
			requests:   4,
			wantLast:   true,
			sleepAfter: 0,
		},
		{
			name:       "Failure: 버스트 초과 시 요청 거부",
			rps:        10,
			burst:      5,
			ttl:        100 * time.Millisecond,
			requests:   6,
			wantLast:   false,
			sleepAfter: 0,
		},
		{
			name:       "Success: 토큰 리필 후 요청 재허용",
			rps:        10,
			burst:      5,
			ttl:        100 * time.Millisecond,
			requests:   5,
			wantLast:   true,
			sleepAfter: 110 * time.Millisecond,
			wantAfter:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limiter := New(tt.rps, tt.burst, tt.ttl)
			defer limiter.Stop()

			key := "user1"
			var result bool
			for range tt.requests {
				result = limiter.Allow(key)
			}
			assert.Equal(t, tt.wantLast, result)

			if tt.sleepAfter > 0 {
				time.Sleep(tt.sleepAfter)
				assert.Equal(t, tt.wantAfter, limiter.Allow(key))
			}
		})
	}
}

func TestTokenBucketLimiter_KeyIsolation(t *testing.T) {
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
			limiter := New(10, 2, time.Minute)
			defer limiter.Stop()

			limiter.Allow("key-a")
			limiter.Allow("key-a")
			assert.False(t, limiter.Allow("key-a"))

			assert.True(t, limiter.Allow("key-b"))
		})
	}
}

func TestTokenBucketLimiter_Concurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		burst         int
		totalRequests int
	}{
		{
			name:          "Success: 동시성 보호 및 정확한 처리율 제한",
			burst:         50,
			totalRequests: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limiter := New(100, tt.burst, time.Hour)
			defer limiter.Stop()

			var wg sync.WaitGroup
			var mu sync.Mutex
			allowedCount := 0

			for range tt.totalRequests {
				wg.Go(func() {
					if limiter.Allow("concurrent_user") {
						mu.Lock()
						allowedCount++
						mu.Unlock()
					}
				})
			}

			wg.Wait()
			assert.LessOrEqual(t, allowedCount, tt.burst)
		})
	}
}
