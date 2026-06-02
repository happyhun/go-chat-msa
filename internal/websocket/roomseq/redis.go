package roomseq

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

const setMaxLua = `
local current = redis.call("GET", KEYS[1])
if current == false or tonumber(current) < tonumber(ARGV[1]) then
    redis.call("SET", KEYS[1], ARGV[1])
    return 1
end
return 0
`

var setMaxScript = redis.NewScript(setMaxLua)

type Store struct {
	client    *redis.Client
	keyPrefix string
}

func NewStore(client *redis.Client, keyPrefix string) *Store {
	return &Store{
		client:    client,
		keyPrefix: keyPrefix,
	}
}

func (s *Store) Get(ctx context.Context, roomID string) (int64, error) {
	if s == nil {
		return 0, errors.New("room sequence floor store is nil")
	}
	value, err := s.client.Get(ctx, s.key(roomID)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get room sequence floor: %w", err)
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse room sequence floor: %w", err)
	}
	return seq, nil
}

func (s *Store) SetMax(ctx context.Context, roomID string, seq int64) error {
	if s == nil {
		return errors.New("room sequence floor store is nil")
	}
	if seq <= 0 {
		return nil
	}
	if err := setMaxScript.Run(ctx, s.client, []string{s.key(roomID)}, seq).Err(); err != nil {
		return fmt.Errorf("set room sequence floor: %w", err)
	}
	return nil
}

func (s *Store) key(roomID string) string {
	return s.keyPrefix + roomID
}
