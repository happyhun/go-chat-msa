package telemetry

import (
	"context"
	"log/slog"
	"sync/atomic"

	"go.mongodb.org/mongo-driver/event"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var mongoPoolMeter = otel.Meter("go-chat-msa/metrics/mongo-pool")

type mongoPoolCollector struct {
	checkedOut int64
	open       int64
	created    uint64
	closed     uint64
}

func NewMongoPoolMonitor() *event.PoolMonitor {
	c := &mongoPoolCollector{}
	c.registerMetrics()

	return &event.PoolMonitor{
		Event: c.handleEvent,
	}
}

func (c *mongoPoolCollector) registerMetrics() {
	var err error
	_, err = mongoPoolMeter.Int64ObservableGauge("gochat_mongo_pool_checked_out_conns",
		metric.WithDescription("Number of currently checked-out (in-use) connections."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(atomic.LoadInt64(&c.checkedOut))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_pool_checked_out_conns", "error", err)
	}
	_, err = mongoPoolMeter.Int64ObservableGauge("gochat_mongo_pool_open_conns",
		metric.WithDescription("Total number of open connections in the pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(atomic.LoadInt64(&c.open))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_pool_open_conns", "error", err)
	}
	_, err = mongoPoolMeter.Int64ObservableCounter("gochat_mongo_pool_created",
		metric.WithDescription("Cumulative number of connections created."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(atomic.LoadUint64(&c.created)))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_pool_created", "error", err)
	}
	_, err = mongoPoolMeter.Int64ObservableCounter("gochat_mongo_pool_closed",
		metric.WithDescription("Cumulative number of connections closed."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(atomic.LoadUint64(&c.closed)))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_pool_closed", "error", err)
	}
}

func (c *mongoPoolCollector) handleEvent(evt *event.PoolEvent) {
	switch evt.Type {
	case event.ConnectionCreated:
		atomic.AddUint64(&c.created, 1)
		atomic.AddInt64(&c.open, 1)
	case event.ConnectionClosed:
		atomic.AddUint64(&c.closed, 1)
		atomic.AddInt64(&c.open, -1)
	case event.GetSucceeded:
		atomic.AddInt64(&c.checkedOut, 1)
	case event.ConnectionReturned:
		atomic.AddInt64(&c.checkedOut, -1)
	}
}
