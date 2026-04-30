package database

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

func NewRedis(addr string) (*redis.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()

	client := redis.NewClient(&redis.Options{Addr: addr})

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("unable to ping redis: %w", err)
	}

	return client, nil
}
