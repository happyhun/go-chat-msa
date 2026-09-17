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
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/telemetry"
	"go-chat-msa/internal/websocket"
	"go-chat-msa/internal/websocket/natsbus"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	userpb "go-chat-msa/api/proto/user/v1"
)

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
		shutdown, err := telemetry.InitOTel(ctx, "websocket-service", cfg.Telemetry.OTelEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize otel", "error", err)
		} else {
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = shutdown(shutdownCtx)
			}()
		}
	}

	if cfg.Telemetry.PyroscopeEndpoint != "" {
		stopProfiler, err := telemetry.InitProfiling("websocket-service", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	userClient, userHealth, cleanupClients, err := initClients(cfg)
	if err != nil {
		return err
	}
	defer cleanupClients()

	redisClient, err := database.NewRedis(cfg.Redis.Addr)
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()

	podName := os.Getenv("POD_NAME")

	bus, err := natsbus.Connect(natsbus.Config{
		URL:             cfg.NATSURL(),
		Name:            podName,
		FlushTimeout:    cfg.WS.NATS.FlushTimeout,
		SubPendingMsgs:  cfg.WS.NATS.SubPendingMsgs,
		SubPendingBytes: cfg.WS.NATS.SubPendingBytes,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := bus.Drain(); err != nil {
			slog.WarnContext(ctx, "failed to drain NATS connection", "error", err)
		}
	}()

	router := websocket.NewRouter(userClient, cfg.WS, bus,
		websocket.WithShutdownTimeout(cfg.ShutdownTimeout),
		websocket.WithInternalSecret(cfg.Internal.Secret),
		websocket.WithPodName(podName),
		websocket.WithRedisClient(redisClient),
		websocket.WithHealthClients(userHealth))

	bus.SetObserver(router.Observer())

	return runServer(ctx, cfg, router)
}

func loadConfig() (*websocket.Config, error) {
	return config.LoadRuntime[websocket.Config]()
}

const grpcRoundRobinServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

func initClients(cfg *websocket.Config) (
	userpb.UserServiceClient,
	grpc_health_v1.HealthClient,
	func(),
	error,
) {
	grpcTimeout := cfg.WS.GRPCClient.Timeout
	opts := []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(grpcRoundRobinServiceConfig),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.WS.GRPCClient.Keepalive.Time,
			Timeout:             cfg.WS.GRPCClient.Keepalive.Timeout,
			PermitWithoutStream: true,
		}),
		grpc.WithChainUnaryInterceptor(
			telemetry.MetricsClientInterceptor("websocket-service"),
			middleware.TimeoutClientInterceptor(grpcTimeout),
		),
	}

	userConn, err := grpc.NewClient(cfg.UserAddr(), opts...)
	if err != nil {
		return nil, nil, nil, err
	}

	cleanupClients := func() {
		_ = userConn.Close()
	}

	return userpb.NewUserServiceClient(userConn),
		grpc_health_v1.NewHealthClient(userConn),
		cleanupClients,
		nil
}

func runServer(ctx context.Context, cfg *websocket.Config, router *websocket.Router) error {
	mux := http.NewServeMux()

	mux.Handle("/", otelhttp.NewMiddleware("websocket-service",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + telemetry.NormalizePath(r.URL.Path)
		}),
	)(
		middleware.RecoveryMiddleware(
			middleware.LoggingMiddleware(
				telemetry.MetricsMiddleware("websocket-service", router),
			),
		),
	))

	srv := &http.Server{
		Addr:              ":" + cfg.Port.WebSocket,
		Handler:           mux,
		ReadHeaderTimeout: cfg.WS.Server.ReadHeaderTimeout,
	}

	eg, ctx := errgroup.WithContext(ctx)
	managerCtx, stopManager := context.WithCancel(context.Background())
	defer stopManager()

	eg.Go(func() error {
		router.RunManager(managerCtx)
		return nil
	})

	eg.Go(func() error {
		slog.InfoContext(ctx, "Starting WebSocket Service", "port", cfg.Port.WebSocket, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	eg.Go(func() error {
		<-ctx.Done()
		slog.InfoContext(ctx, "Shutting down WebSocket Service...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()

		err := srv.Shutdown(shutdownCtx)
		stopManager()
		if err != nil {
			return err
		}

		slog.InfoContext(ctx, "WebSocket Service stopped gracefully")
		return nil
	})

	return eg.Wait()
}
