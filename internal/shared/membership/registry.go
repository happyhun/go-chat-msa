package membership

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const deregisterTimeout = 2 * time.Second

type Registry struct {
	client    *redis.Client
	keyPrefix string
	addr      string
	ttl       time.Duration
	heartbeat time.Duration
}

func NewRegistry(client *redis.Client, keyPrefix, addr string, ttl, heartbeat time.Duration) *Registry {
	return &Registry{
		client:    client,
		keyPrefix: keyPrefix,
		addr:      addr,
		ttl:       ttl,
		heartbeat: heartbeat,
	}
}

func (r *Registry) key() string {
	return r.keyPrefix + r.addr
}

func (r *Registry) Run(ctx context.Context) error {
	if err := r.client.Set(ctx, r.key(), r.addr, r.ttl).Err(); err != nil {
		return fmt.Errorf("initial register: %w", err)
	}
	slog.InfoContext(ctx, "membership registered",
		"addr", r.addr, "ttl_seconds", int(r.ttl.Seconds()))

	ticker := time.NewTicker(r.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.deregister()
			return nil
		case <-ticker.C:
			if err := r.client.Expire(ctx, r.key(), r.ttl).Err(); err != nil {
				slog.WarnContext(ctx, "membership heartbeat failed (fail-open)",
					"addr", r.addr, "error", err)
			}
		}
	}
}

func (r *Registry) deregister() {
	ctx, cancel := context.WithTimeout(context.Background(), deregisterTimeout)
	defer cancel()
	if err := r.client.Del(ctx, r.key()).Err(); err != nil {
		slog.WarnContext(ctx, "membership deregister failed",
			"addr", r.addr, "error", err)
		return
	}
	slog.InfoContext(ctx, "membership deregistered", "addr", r.addr)
}
