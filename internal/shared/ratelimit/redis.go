package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
)

type RedisLimiter struct {
	limiter *redis_rate.Limiter
	rate    int
	burst   int
}

func NewRedis(client *redis.Client, rate int, burst int) *RedisLimiter {
	return &RedisLimiter{
		limiter: redis_rate.NewLimiter(client),
		rate:    rate,
		burst:   burst,
	}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) (bool, error) {
	res, err := l.limiter.Allow(ctx, key, redis_rate.Limit{
		Rate:   l.rate,
		Burst:  l.burst,
		Period: time.Second,
	})
	if err != nil {
		return false, fmt.Errorf("unable to check rate limit: %w", err)
	}
	return res.Allowed >= 1, nil
}
