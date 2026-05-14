package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"go-chat-msa/internal/retention"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/shared/telemetry"
	userdb "go-chat-msa/internal/user/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "retention-job failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := retention.LoadConfig()
	if err != nil {
		return err
	}

	if cfg.Telemetry.OTelEndpoint != "" {
		shutdown, err := telemetry.InitOTel(ctx, "retention-job", cfg.Telemetry.OTelEndpoint)
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

		stopProfiler, err := telemetry.InitProfiling("retention-job", cfg.Telemetry.PyroscopeEndpoint)
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

	jobCtx, cancel := context.WithTimeout(ctx, cfg.RetentionWorker.JobTimeout)
	defer cancel()

	startedAt := time.Now()
	slog.InfoContext(jobCtx, "retention-job started",
		"retention_days", cfg.RetentionWorker.RetentionDays,
		"timeout", cfg.RetentionWorker.JobTimeout,
	)

	result, err := retention.PurgeDeleted(jobCtx, queries, cfg.RetentionWorker.RetentionDays, startedAt)
	duration := time.Since(startedAt)
	retention.RecordMetrics(jobCtx, result, duration)
	retention.LogResult(jobCtx, result)
	if err != nil {
		return err
	}

	slog.InfoContext(jobCtx, "retention-job completed",
		"duration", duration,
		"rooms", result.Rooms.Count,
		"users", result.Users.Count,
	)
	return nil
}
