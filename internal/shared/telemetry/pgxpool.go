package telemetry

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var pgxpoolMeter = otel.Meter("go-chat-msa/metrics/pgxpool")

func RegisterPgxpoolMetrics(pool *pgxpool.Pool) {
	var err error
	_, err = pgxpoolMeter.Int64ObservableGauge("gochat_pgxpool_acquired_conns",
		metric.WithDescription("Number of currently acquired (in-use) connections."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().AcquiredConns()))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_acquired_conns", "error", err)
	}
	_, err = pgxpoolMeter.Int64ObservableGauge("gochat_pgxpool_idle_conns",
		metric.WithDescription("Number of idle connections in the pool."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().IdleConns()))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_idle_conns", "error", err)
	}
	_, err = pgxpoolMeter.Int64ObservableGauge("gochat_pgxpool_total_conns",
		metric.WithDescription("Total number of connections (idle + acquired + constructing)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().TotalConns()))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_total_conns", "error", err)
	}
	_, err = pgxpoolMeter.Int64ObservableGauge("gochat_pgxpool_max_conns",
		metric.WithDescription("Maximum number of connections allowed by pool config."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(pool.Stat().MaxConns()))
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_max_conns", "error", err)
	}
	_, err = pgxpoolMeter.Int64ObservableCounter("gochat_pgxpool_acquire_count",
		metric.WithDescription("Cumulative number of successful connection acquisitions."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(pool.Stat().AcquireCount())
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_acquire_count", "error", err)
	}
	_, err = pgxpoolMeter.Float64ObservableCounter("gochat_pgxpool_acquire_duration_seconds",
		metric.WithDescription("Cumulative time spent acquiring connections (seconds)."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(pool.Stat().AcquireDuration().Seconds())
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_acquire_duration_seconds", "error", err)
	}
	_, err = pgxpoolMeter.Int64ObservableCounter("gochat_pgxpool_empty_acquire_count",
		metric.WithDescription("Cumulative acquires that had to wait because the pool was empty."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(pool.Stat().EmptyAcquireCount())
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_empty_acquire_count", "error", err)
	}
	_, err = pgxpoolMeter.Int64ObservableCounter("gochat_pgxpool_canceled_acquire_count",
		metric.WithDescription("Cumulative acquires canceled by context before a connection was obtained."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(pool.Stat().CanceledAcquireCount())
			return nil
		}),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pgxpool_canceled_acquire_count", "error", err)
	}
}
