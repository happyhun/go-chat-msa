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

	"go-chat-msa/internal/apigateway"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/telemetry"

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
		shutdown, err := telemetry.InitOTel(ctx, "api-gateway", cfg.Telemetry.OTelEndpoint)
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

		stopProfiler, err := telemetry.InitProfiling("api-gateway", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	userClient, chatClient, cleanupClients, err := initClients(cfg)
	if err != nil {
		return err
	}
	defer cleanupClients()

	router := apigateway.NewRouter(cfg, userClient, chatClient)

	return runServer(ctx, cfg, router)
}

func loadConfig() (*apigateway.Config, error) {
	env := config.GetEnv()
	logger.InitLogger(env)

	return config.Load[apigateway.Config]("configs", "base", env)
}

func initClients(cfg *apigateway.Config) (userpb.UserServiceClient, chatpb.ChatServiceClient, func(), error) {
	grpcTimeout := cfg.APIGateway.GRPCClient.Timeout
	opts := []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.APIGateway.GRPCClient.Keepalive.Time,
			Timeout:             cfg.APIGateway.GRPCClient.Keepalive.Timeout,
			PermitWithoutStream: true,
		}),
		grpc.WithChainUnaryInterceptor(
			telemetry.MetricsClientInterceptor("api-gateway"),
			middleware.TimeoutClientInterceptor(grpcTimeout),
		),
	}

	userConn, err := grpc.NewClient(cfg.UserAddr(), opts...)
	if err != nil {
		return nil, nil, nil, err
	}

	chatConn, err := grpc.NewClient(cfg.ChatAddr(), opts...)
	if err != nil {
		userConn.Close()
		return nil, nil, nil, err
	}

	cleanupClients := func() {
		userConn.Close()
		chatConn.Close()
	}

	return userpb.NewUserServiceClient(userConn), chatpb.NewChatServiceClient(chatConn), cleanupClients, nil
}

func runServer(ctx context.Context, cfg *apigateway.Config, router *apigateway.Router) error {
	mux := http.NewServeMux()

	mux.Handle("/", otelhttp.NewMiddleware("api-gateway",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + telemetry.NormalizePath(r.URL.Path)
		}),
	)(
		middleware.RecoveryMiddleware(
			middleware.LoggingMiddleware(
				telemetry.MetricsMiddleware("api-gateway", router),
			),
		),
	))

	srv := &http.Server{
		Addr:         ":" + cfg.Port.APIGateway,
		Handler:      mux,
		ReadTimeout:  cfg.APIGateway.Server.ReadTimeout,
		WriteTimeout: cfg.APIGateway.Server.WriteTimeout,
		IdleTimeout:  cfg.APIGateway.Server.IdleTimeout,
	}

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		slog.InfoContext(ctx, "Starting API Gateway",
			"addr", srv.Addr,
			"env", cfg.Env,
		)

		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	eg.Go(func() error {
		<-ctx.Done()
		slog.InfoContext(ctx, "Shutting down API Gateway...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()

		err := srv.Shutdown(shutdownCtx)

		slog.InfoContext(ctx, "Waiting for background goroutines to finish...")
		router.Stop()
		router.Wait()

		slog.InfoContext(ctx, "API Gateway stopped gracefully")
		return err
	})

	return eg.Wait()
}
