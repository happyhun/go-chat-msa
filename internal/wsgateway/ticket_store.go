package wsgateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const ticketKeyPrefix = "ws:ticket:"

type TicketStore struct {
	client *redis.Client
}

func NewTicketStore(client *redis.Client) *TicketStore {
	return &TicketStore{client: client}
}

func (s *TicketStore) Set(ctx context.Context, ticket, userID string, ttl time.Duration) error {
	ok, err := s.client.SetNX(ctx, ticketKeyPrefix+ticket, userID, ttl).Result()
	if err != nil {
		return fmt.Errorf("unable to set ticket: %w", err)
	}
	if !ok {
		return errors.New("ticket already exists")
	}
	return nil
}

func (s *TicketStore) GetAndDelete(ctx context.Context, ticket string) (string, bool, error) {
	userID, err := s.client.GetDel(ctx, ticketKeyPrefix+ticket).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("unable to get and delete ticket: %w", err)
	}
	return userID, true, nil
}
