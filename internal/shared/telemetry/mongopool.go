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
	checkedOut atomic.Int64
	open       atomic.Int64
	created    atomic.Int64
	closed     atomic.Int64
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
			o.Observe(c.checkedOut.Load())
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_pool_checked_out_conns", "error", err)
	}
	_, err = mongoPoolMeter.Int64ObservableGauge("gochat_mongo_pool_open_conns",
		metric.WithDescription("Total number of open connections in the pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(c.open.Load())
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_pool_open_conns", "error", err)
	}
	_, err = mongoPoolMeter.Int64ObservableCounter("gochat_mongo_pool_created",
		metric.WithDescription("Cumulative number of connections created."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(c.created.Load())
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_pool_created", "error", err)
	}
	_, err = mongoPoolMeter.Int64ObservableCounter("gochat_mongo_pool_closed",
		metric.WithDescription("Cumulative number of connections closed."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(c.closed.Load())
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
		c.created.Add(1)
		c.open.Add(1)
	case event.ConnectionClosed:
		c.closed.Add(1)
		c.open.Add(-1)
	case event.GetSucceeded:
		c.checkedOut.Add(1)
	case event.ConnectionReturned:
		c.checkedOut.Add(-1)
	}
}
