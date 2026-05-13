package membership

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	reconcileInterval = 30 * time.Second
	scanCount         = 100
)

type RingUpdater interface {
	Set(addrs []string)
}

type Watcher struct {
	client    *redis.Client
	keyPrefix string
	ring      RingUpdater
	forceCh   chan struct{}
	events    chan struct{}

	lastAddrs map[string]struct{}
}

func NewWatcher(client *redis.Client, keyPrefix string, ring RingUpdater) *Watcher {
	return &Watcher{
		client:    client,
		keyPrefix: keyPrefix,
		ring:      ring,
		forceCh:   make(chan struct{}, 1),
		events:    make(chan struct{}, 1),
	}
}

func (w *Watcher) Events() <-chan struct{} { return w.events }

func (w *Watcher) ForceReconcile() {
	select {
	case w.forceCh <- struct{}{}:
	default:
	}
}

func (w *Watcher) Run(ctx context.Context) error {
	db := w.client.Options().DB
	pattern := fmt.Sprintf("__keyspace@%d__:%s*", db, w.keyPrefix)

	pubsub := w.client.PSubscribe(ctx, pattern)
	defer func() { _ = pubsub.Close() }()

	if err := w.reconcile(ctx); err != nil {
		slog.WarnContext(ctx, "membership initial reconcile failed (fail-open)",
			"error", err)
	}

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	pubsubCh := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pubsubCh:
			w.runReconcile(ctx)
		case <-ticker.C:
			w.runReconcile(ctx)
		case <-w.forceCh:
			w.runReconcile(ctx)
		}
	}
}

func (w *Watcher) runReconcile(ctx context.Context) {
	if err := w.reconcile(ctx); err != nil {
		membershipReconcileTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		slog.WarnContext(ctx, "membership reconcile failed (fail-open)", "error", err)
		return
	}
	membershipReconcileTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
	w.notify()
}

func (w *Watcher) reconcile(ctx context.Context) error {
	addrs, err := w.scanMembers(ctx)
	if err != nil {
		return err
	}
	w.ring.Set(addrs)
	if w.membershipChanged(addrs) {
		slog.InfoContext(ctx, "membership ring reconciled",
			"members", addrs, "count", len(addrs))
	}
	return nil
}

func (w *Watcher) membershipChanged(addrs []string) bool {
	if len(addrs) != len(w.lastAddrs) {
		w.lastAddrs = toSet(addrs)
		return true
	}
	for _, a := range addrs {
		if _, ok := w.lastAddrs[a]; !ok {
			w.lastAddrs = toSet(addrs)
			return true
		}
	}
	return false
}

func toSet(addrs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		out[a] = struct{}{}
	}
	return out
}

func (w *Watcher) scanMembers(ctx context.Context) ([]string, error) {
	var cursor uint64
	pattern := w.keyPrefix + "*"
	addrs := make([]string, 0, 8)
	for {
		keys, next, err := w.client.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("scan members: %w", err)
		}
		for _, k := range keys {
			addrs = append(addrs, strings.TrimPrefix(k, w.keyPrefix))
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return addrs, nil
}

func (w *Watcher) notify() {
	select {
	case w.events <- struct{}{}:
	default:
	}
}
