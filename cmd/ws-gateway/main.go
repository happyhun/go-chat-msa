package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/membership"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/telemetry"
	"go-chat-msa/internal/wsgateway"
	"go-chat-msa/internal/wsgateway/loadbalance"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"
)

const membershipKeyPrefix = "wss:member:"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.ErrorContext(context.Background(), "application failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger.InitLogger(cfg.Env)

	if cfg.Telemetry.OTelEndpoint != "" {
		shutdown, err := telemetry.InitOTel(ctx, "ws-gateway", cfg.Telemetry.OTelEndpoint)
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
		stopProfiler, err := telemetry.InitProfiling("ws-gateway", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	redisClient, err := database.NewRedis(cfg.Redis.Addr)
	if err != nil {
		return err
	}
	defer redisClient.Close()

	hashRing := loadbalance.New(nil)
	watcher := membership.NewWatcher(redisClient, membershipKeyPrefix, hashRing)
	router := wsgateway.NewRouter(cfg, hashRing, watcher, redisClient)

	return runServer(ctx, cfg, router, watcher)
}

func loadConfig() (*wsgateway.Config, error) {
	return config.LoadRuntime[wsgateway.Config]()
}

func runServer(ctx context.Context, cfg *wsgateway.Config, router *wsgateway.Router, watcher *membership.Watcher) error {
	mux := http.NewServeMux()

	mux.Handle("/", otelhttp.NewMiddleware("ws-gateway",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + telemetry.NormalizePath(r.URL.Path)
		}),
	)(
		middleware.RecoveryMiddleware(
			middleware.LoggingMiddleware(
				telemetry.MetricsMiddleware("ws-gateway", router),
			),
		),
	))

	srv := &http.Server{
		Addr:              ":" + cfg.Port.WSGateway,
		Handler:           mux,
		ReadHeaderTimeout: cfg.WSGateway.Server.ReadHeaderTimeout,
	}

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		return watcher.Run(ctx)
	})

	eg.Go(func() error {
		slog.InfoContext(ctx, "Starting WebSocket Gateway", "port", cfg.Port.WSGateway, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	eg.Go(func() error {
		<-ctx.Done()
		slog.InfoContext(ctx, "Shutting down WebSocket Gateway...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()

		err := srv.Shutdown(shutdownCtx)

		router.Stop()

		slog.InfoContext(ctx, "WebSocket Gateway stopped gracefully")
		return err
	})

	return eg.Wait()
}
