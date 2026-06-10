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
	"go-chat-msa/internal/websocket"
	"go-chat-msa/internal/websocket/roomlease"
	"go-chat-msa/internal/websocket/roomseq"
	"go-chat-msa/internal/wsgateway/loadbalance"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
)

const (
	membershipKeyPrefix = "wss:member:"
	membershipTTL       = 30 * time.Second
	membershipHeartbeat = 10 * time.Second
	roomLeaseKeyPrefix  = "wss:room:lease:"
	sequenceFloorPrefix = "wss:room:seqfloor:"
	roomLeaseTTL        = 30 * time.Second
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
				shutdown(shutdownCtx)
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

	chatClient, userClient, chatHealth, userHealth, cleanupClients, err := initClients(cfg)
	if err != nil {
		return err
	}
	defer cleanupClients()

	redisClient, err := database.NewRedis(cfg.Redis.Addr)
	if err != nil {
		return err
	}
	defer redisClient.Close()

	hashRing := loadbalance.New(nil)
	registry := membership.NewRegistry(redisClient, membershipKeyPrefix, cfg.WS.AdvertisedAddr, membershipTTL, membershipHeartbeat)
	watcher := membership.NewWatcher(redisClient, membershipKeyPrefix, hashRing)
	leaseStore := roomlease.NewStore(redisClient, roomLeaseKeyPrefix, cfg.WS.AdvertisedAddr, roomLeaseTTL)
	sequenceFloorStore := roomseq.NewStore(redisClient, sequenceFloorPrefix)

	router := websocket.NewRouter(chatClient, userClient, cfg.WS, hashRing,
		websocket.WithShutdownTimeout(cfg.ShutdownTimeout),
		websocket.WithRedisClient(redisClient),
		websocket.WithRoomLeaseStore(leaseStore),
		websocket.WithSequenceFloorStore(sequenceFloorStore),
		websocket.WithHealthClients(chatHealth, userHealth))

	return runServer(ctx, cfg, router, registry, watcher)
}

func loadConfig() (*websocket.Config, error) {
	return config.LoadRuntime[websocket.Config]()
}

const grpcRoundRobinServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

func initClients(cfg *websocket.Config) (
	chatpb.ChatServiceClient,
	userpb.UserServiceClient,
	grpc_health_v1.HealthClient,
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

	chatConn, err := grpc.NewClient(cfg.ChatAddr(), opts...)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	userConn, err := grpc.NewClient(cfg.UserAddr(), opts...)
	if err != nil {
		chatConn.Close()
		return nil, nil, nil, nil, nil, err
	}

	cleanupClients := func() {
		chatConn.Close()
		userConn.Close()
	}

	return chatpb.NewChatServiceClient(chatConn),
		userpb.NewUserServiceClient(userConn),
		grpc_health_v1.NewHealthClient(chatConn),
		grpc_health_v1.NewHealthClient(userConn),
		cleanupClients,
		nil
}

func runServer(
	ctx context.Context,
	cfg *websocket.Config,
	router *websocket.Router,
	registry *membership.Registry,
	watcher *membership.Watcher,
) error {
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

	eg.Go(func() error {
		return registry.Run(ctx)
	})

	eg.Go(func() error {
		return watcher.Run(ctx)
	})

	eg.Go(func() error {
		router.RunManager(ctx)
		return nil
	})

	eg.Go(func() error {
		router.WatchOwnership(ctx, watcher.Events())
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

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}

		slog.InfoContext(ctx, "WebSocket Service stopped gracefully")
		return nil
	})

	return eg.Wait()
}
