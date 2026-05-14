package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/telemetry"
	"go-chat-msa/internal/user"
	"go-chat-msa/internal/user/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "user-token-purge-job failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if cfg.Telemetry.OTelEndpoint != "" {
		shutdown, err := telemetry.InitOTel(ctx, "user-token-purge-job", cfg.Telemetry.OTelEndpoint)
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

		stopProfiler, err := telemetry.InitProfiling("user-token-purge-job", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	pgPool, err := database.NewPostgres(cfg.DB.PostgresURL)
	if err != nil {
		return err
	}
	defer pgPool.Close()

	telemetry.RegisterPgxpoolMetrics(pgPool)

	queries := db.New(telemetry.InstrumentedDBTX(pgPool))

	jobCtx, cancel := context.WithTimeout(ctx, cfg.ShutdownTimeout)
	defer cancel()

	slog.InfoContext(jobCtx, "user-token-purge-job started", "timeout", cfg.ShutdownTimeout)
	if err := user.PurgeExpiredTokensOnce(jobCtx, queries); err != nil {
		return err
	}
	slog.InfoContext(jobCtx, "user-token-purge-job completed")
	return nil
}

func loadConfig() (*user.Config, error) {
	env := config.GetEnv()
	logger.InitLogger(env)

	return config.Load[user.Config]("configs", "base", env)
}
