package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/telemetry"
	"go-chat-msa/internal/websocket"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	chatpb "go-chat-msa/api/proto/chat/v1"
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
		runtime.SetMutexProfileFraction(10)
		runtime.SetBlockProfileRate(10000)

		stopProfiler, err := telemetry.InitProfiling("websocket-service", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	chatClient, userClient, cleanupClients, err := initClients(cfg)
	if err != nil {
		return err
	}
	defer cleanupClients()

	router := websocket.NewRouter(chatClient, userClient, cfg.WS)

	return runServer(ctx, cfg, router)
}

func loadConfig() (*websocket.Config, error) {
	env := config.GetEnv()
	logger.InitLogger(env)

	return config.Load[websocket.Config]("configs", "base", env)
}

func initClients(cfg *websocket.Config) (chatpb.ChatServiceClient, userpb.UserServiceClient, func(), error) {
	grpcTimeout := cfg.WS.GRPCClient.Timeout
	opts := []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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
		return nil, nil, nil, err
	}

	userConn, err := grpc.NewClient(cfg.UserAddr(), opts...)
	if err != nil {
		chatConn.Close()
		return nil, nil, nil, err
	}

	cleanupClients := func() {
		chatConn.Close()
		userConn.Close()
	}

	return chatpb.NewChatServiceClient(chatConn), userpb.NewUserServiceClient(userConn), cleanupClients, nil
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

	eg.Go(func() error {
		router.RunManager(ctx)
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

