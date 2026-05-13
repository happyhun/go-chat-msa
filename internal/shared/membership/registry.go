package membership

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	deregisterTimeout = 2 * time.Second
	tokenByteLength   = 16
)

var compareAndDeleteScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)

type Registry struct {
	client    *redis.Client
	keyPrefix string
	addr      string
	token     string
	ttl       time.Duration
	heartbeat time.Duration
}

func NewRegistry(client *redis.Client, keyPrefix, addr string, ttl, heartbeat time.Duration) (*Registry, error) {
	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generate lease token: %w", err)
	}
	return &Registry{
		client:    client,
		keyPrefix: keyPrefix,
		addr:      addr,
		token:     token,
		ttl:       ttl,
		heartbeat: heartbeat,
	}, nil
}

func generateToken() (string, error) {
	buf := make([]byte, tokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (r *Registry) key() string {
	return r.keyPrefix + r.addr
}

func (r *Registry) refresh(ctx context.Context) error {
	return r.client.Set(ctx, r.key(), r.token, r.ttl).Err()
}

func (r *Registry) Run(ctx context.Context) error {
	if err := r.refresh(ctx); err != nil {
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
			if err := r.refresh(ctx); err != nil {
				slog.WarnContext(ctx, "membership heartbeat failed (fail-open)",
					"addr", r.addr, "error", err)
			}
		}
	}
}

func (r *Registry) deregister() {
	ctx, cancel := context.WithTimeout(context.Background(), deregisterTimeout)
	defer cancel()
	result, err := compareAndDeleteScript.Run(ctx, r.client, []string{r.key()}, r.token).Int64()
	if err != nil {
		slog.WarnContext(ctx, "membership deregister failed",
			"addr", r.addr, "error", err)
		return
	}
	if result == 0 {
		slog.WarnContext(ctx, "membership deregister skipped (token mismatch or already gone)",
			"addr", r.addr)
		return
	}
	slog.InfoContext(ctx, "membership deregistered", "addr", r.addr)
}
