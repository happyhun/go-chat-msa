package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robfig/cron/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/telemetry"
	userdb "go-chat-msa/internal/user/db"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry        config.TelemetryConfig       `mapstructure:"TELEMETRY"`
	Port             config.PortConfig            `mapstructure:"PORT"             validate:"required"`
	DB               DBConfig                     `mapstructure:"DB"               validate:"required"`
	RetentionWorker  config.RetentionWorkerConfig `mapstructure:"RETENTION_WORKER" validate:"required"`
}

type DBConfig struct {
	PostgresURL string `mapstructure:"POSTGRES_URL" validate:"required"`
}

var retentionMeter = otel.Meter("go-chat-msa/retention-worker")

var (
	retentionDuration  metric.Float64Histogram
	retentionPurgedTotal metric.Int64Counter
)

func init() {
	retentionDuration, _ = retentionMeter.Float64Histogram("gochat_retention_duration_seconds",
		metric.WithDescription("리텐션 퍼지 작업 소요 시간"),
		metric.WithExplicitBucketBoundaries(.1, .25, .5, 1, 2.5, 5, 10, 30),
	)
	retentionPurgedTotal, _ = retentionMeter.Int64Counter("gochat_retention_purged",
		metric.WithDescription("리텐션 퍼지 실행 횟수"),
	)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "retention-worker failed", "error", err)
		os.Exit(1)
	}
}

func purge(ctx context.Context, kind string, fn func(context.Context) (int64, error)) {
	n, err := fn(ctx)
	if err != nil {
		retentionPurgedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("kind", kind),
			attribute.String("status", "error"),
		))
		slog.ErrorContext(ctx, "failed to purge", "kind", kind, "error", err)
		return
	}
	retentionPurgedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("status", "ok"),
	))
	slog.InfoContext(ctx, "purged", "kind", kind, "count", n)
}

func loadConfig() (*Config, error) {
	env := config.GetEnv()
	logger.InitLogger(env)

	return config.Load[Config]("configs", "base", env)
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if cfg.Telemetry.OTelEndpoint != "" {
		shutdown, err := telemetry.InitOTel(ctx, "retention-worker", cfg.Telemetry.OTelEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize otel", "error", err)
		} else {
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				shutdown(shutdownCtx)
			}()
		}
	}

	if cfg.Telemetry.PyroscopeEndpoint != "" {
		runtime.SetMutexProfileFraction(10)
		runtime.SetBlockProfileRate(10000)

		stopProfiler, err := telemetry.InitProfiling("retention-worker", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	pool, err := database.NewPostgres(cfg.DB.PostgresURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	queries := userdb.New(pool)

	c := cron.New()
	if _, err := c.AddFunc(cfg.RetentionWorker.Schedule, func() {
		jobCtx, cancel := context.WithTimeout(context.Background(), cfg.RetentionWorker.JobTimeout)
		defer cancel()

		now := time.Now()
		threshold := pgtype.Timestamptz{
			Time:  now.AddDate(0, 0, -cfg.RetentionWorker.RetentionDays),
			Valid: true,
		}

		purge(jobCtx, "rooms", func(ctx context.Context) (int64, error) {
			return queries.PurgeDeletedRooms(ctx, threshold)
		})
		purge(jobCtx, "users", func(ctx context.Context) (int64, error) {
			return queries.PurgeDeletedUsers(ctx, threshold)
		})

		retentionDuration.Record(jobCtx, time.Since(now).Seconds())
	}); err != nil {
		return err
	}

	c.Start()
	slog.InfoContext(ctx, "retention-worker started",
		"schedule", cfg.RetentionWorker.Schedule,
		"retention_days", cfg.RetentionWorker.RetentionDays,
	)

	<-ctx.Done()
	slog.InfoContext(ctx, "retention-worker shutting down")
	stopCtx := c.Stop()
	select {
	case <-stopCtx.Done():
		slog.InfoContext(ctx, "retention-worker stopped gracefully")
	case <-time.After(cfg.ShutdownTimeout):
		slog.WarnContext(ctx, "retention-worker shutdown timed out")
	}
	return nil
}
