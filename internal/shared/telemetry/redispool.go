package telemetry

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var redisPoolMeter = otel.Meter("go-chat-msa/metrics/redis-pool")

func RegisterRedisPoolMetrics(client *redis.Client) {
	var err error
	_, err = redisPoolMeter.Int64ObservableGauge("gochat_redis_pool_total_conns",
		metric.WithDescription("Total number of connections in the pool (idle + stale + in-use)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(client.PoolStats().TotalConns))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_redis_pool_total_conns", "error", err)
	}
	_, err = redisPoolMeter.Int64ObservableGauge("gochat_redis_pool_idle_conns",
		metric.WithDescription("Number of idle connections in the pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(client.PoolStats().IdleConns))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_redis_pool_idle_conns", "error", err)
	}
	_, err = redisPoolMeter.Int64ObservableGauge("gochat_redis_pool_stale_conns",
		metric.WithDescription("Number of stale connections removed from the pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(client.PoolStats().StaleConns))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_redis_pool_stale_conns", "error", err)
	}
	_, err = redisPoolMeter.Int64ObservableCounter("gochat_redis_pool_hits",
		metric.WithDescription("Cumulative number of times a free connection was found in the pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(client.PoolStats().Hits))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_redis_pool_hits", "error", err)
	}
	_, err = redisPoolMeter.Int64ObservableCounter("gochat_redis_pool_misses",
		metric.WithDescription("Cumulative number of times a free connection was NOT found in the pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(client.PoolStats().Misses))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_redis_pool_misses", "error", err)
	}
	_, err = redisPoolMeter.Int64ObservableCounter("gochat_redis_pool_timeouts",
		metric.WithDescription("Cumulative number of times a wait timeout occurred while obtaining a connection."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(client.PoolStats().Timeouts))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_redis_pool_timeouts", "error", err)
	}
}
