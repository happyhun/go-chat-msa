package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
	"go-chat-msa/internal/retention"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/shared/telemetry"
	userdb "go-chat-msa/internal/user/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "retention-worker failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := retention.LoadConfig()
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
		result, err := retention.PurgeDeleted(jobCtx, queries, cfg.RetentionWorker.RetentionDays, now)
		duration := time.Since(now)
		retention.RecordMetrics(jobCtx, result, duration)
		retention.LogResult(jobCtx, result)
		if err != nil {
			slog.ErrorContext(jobCtx, "retention purge failed", "error", err)
			return
		}
		slog.InfoContext(jobCtx, "retention purge completed", "duration", duration)
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
