package roomlease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	ErrBusy = errors.New("room lease busy")
	ErrLost = errors.New("room lease lost")
)

const (
	fieldOwnerAddr = "owner_addr"
	fieldToken     = "token"
)

const acquireLua = `
if redis.call("EXISTS", KEYS[1]) == 0 then
    redis.call("HSET", KEYS[1], ARGV[1], ARGV[2], ARGV[3], ARGV[4])
    redis.call("PEXPIRE", KEYS[1], ARGV[5])
    return 1
end
return 0
`

const renewLua = `
if redis.call("HGET", KEYS[1], ARGV[1]) == ARGV[2] then
    redis.call("PEXPIRE", KEYS[1], ARGV[3])
    return 1
end
if redis.call("EXISTS", KEYS[1]) == 0 then
    return -1
end
return 0
`

const releaseLua = `
if redis.call("HGET", KEYS[1], ARGV[1]) == ARGV[2] then
    return redis.call("DEL", KEYS[1])
end
return 0
`

var (
	acquireScript = redis.NewScript(acquireLua)
	releaseScript = redis.NewScript(releaseLua)
)

type Lease struct {
	RoomID    string
	OwnerAddr string
	Token     string
}

type RenewResult struct {
	Lease *Lease
	Err   error
}

type Store struct {
	client    *redis.Client
	keyPrefix string
	ownerAddr string
	ttl       time.Duration
}

func NewStore(client *redis.Client, keyPrefix, ownerAddr string, ttl time.Duration) *Store {
	return &Store{
		client:    client,
		keyPrefix: keyPrefix,
		ownerAddr: ownerAddr,
		ttl:       ttl,
	}
}

func (s *Store) Acquire(ctx context.Context, roomID string) (*Lease, error) {
	if s == nil {
		return nil, errors.New("room lease store is nil")
	}
	token := uuid.NewString()
	result, err := acquireScript.Run(ctx, s.client, []string{s.key(roomID)},
		fieldOwnerAddr, s.ownerAddr,
		fieldToken, token,
		s.ttl.Milliseconds(),
	).Int64()
	if err != nil {
		roomLeaseAcquireTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return nil, fmt.Errorf("acquire room lease: %w", err)
	}
	if result == 0 {
		roomLeaseAcquireTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "busy")))
		return nil, ErrBusy
	}
	roomLeaseAcquireTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
	return &Lease{
		RoomID:    roomID,
		OwnerAddr: s.ownerAddr,
		Token:     token,
	}, nil
}

func (s *Store) RenewBatch(ctx context.Context, leases []*Lease) []RenewResult {
	results := make([]RenewResult, 0, len(leases))
	if s == nil || len(leases) == 0 {
		return results
	}

	pipe := s.client.Pipeline()
	cmds := make([]*redis.Cmd, 0, len(leases))
	pending := make([]*Lease, 0, len(leases))
	for _, lease := range leases {
		if lease == nil {
			continue
		}
		cmd := pipe.Eval(ctx, renewLua, []string{s.key(lease.RoomID)},
			fieldToken, lease.Token, s.ttl.Milliseconds())
		cmds = append(cmds, cmd)
		pending = append(pending, lease)
	}

	_, execErr := pipe.Exec(ctx)
	if execErr != nil && !errors.Is(execErr, redis.Nil) {
		slog.WarnContext(ctx, "room lease renew pipeline failed", "error", execErr)
	}

	for i, cmd := range cmds {
		lease := pending[i]
		result, err := cmd.Int64()
		if err != nil {
			roomLeaseRenewTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			results = append(results, RenewResult{Lease: lease, Err: fmt.Errorf("renew room lease: %w", err)})
			continue
		}
		if result != 1 {
			roomLeaseRenewTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "lost")))
			results = append(results, RenewResult{Lease: lease, Err: ErrLost})
			continue
		}
		roomLeaseRenewTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
		results = append(results, RenewResult{Lease: lease})
	}
	return results
}

func (s *Store) Release(ctx context.Context, lease *Lease) error {
	if s == nil || lease == nil {
		return nil
	}
	result, err := releaseScript.Run(ctx, s.client, []string{s.key(lease.RoomID)}, fieldToken, lease.Token).Int64()
	if err != nil {
		return fmt.Errorf("release room lease: %w", err)
	}
	if result == 0 {
		return ErrLost
	}
	return nil
}

func (s *Store) key(roomID string) string {
	return s.keyPrefix + roomID
}

var (
	roomLeaseMeter        = otel.Meter("go-chat-msa/websocket/roomlease")
	roomLeaseAcquireTotal metric.Int64Counter
	roomLeaseRenewTotal   metric.Int64Counter
)

func init() {
	var err error
	roomLeaseAcquireTotal, err = roomLeaseMeter.Int64Counter("gochat_ws_room_lease_acquire_total",
		metric.WithDescription("room lease acquire attempts"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_room_lease_acquire_total", "error", err)
	}
	roomLeaseRenewTotal, err = roomLeaseMeter.Int64Counter("gochat_ws_room_lease_renew_total",
		metric.WithDescription("room lease renew attempts"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_room_lease_renew_total", "error", err)
	}
}
