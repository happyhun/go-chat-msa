package wsticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const keyPrefix = "ws:ticket:"

type Store struct {
	client *redis.Client
}

func NewStore(client *redis.Client) *Store {
	return &Store{client: client}
}

func (s *Store) Issue(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	ticket := uuid.NewString()
	ok, err := s.client.SetNX(ctx, keyPrefix+ticket, userID, ttl).Result()
	if err != nil {
		return "", fmt.Errorf("set ticket: %w", err)
	}
	if !ok {
		return "", errors.New("ticket already exists")
	}
	return ticket, nil
}

func (s *Store) Consume(ctx context.Context, ticket string) (string, bool, error) {
	userID, err := s.client.GetDel(ctx, keyPrefix+ticket).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("consume ticket: %w", err)
	}
	return userID, true, nil
}
