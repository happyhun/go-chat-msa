package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	pb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/chat"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
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
		shutdown, err := telemetry.InitOTel(ctx, "chat-service", cfg.Telemetry.OTelEndpoint)
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

		stopProfiler, err := telemetry.InitProfiling("chat-service", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	mongoClient, err := database.NewMongo(cfg.DB.MongoURI, database.WithPoolMonitor(telemetry.NewMongoPoolMonitor()))
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := mongoClient.Disconnect(cleanupCtx); err != nil {
			slog.ErrorContext(ctx, "failed to disconnect mongo", "error", err)
		}
	}()

	msgCol := mongoClient.Database("chat_service").Collection("messages")
	repo := chat.NewRepository(telemetry.NewInstrumentedCollection(msgCol))
	chatService := chat.NewService(repo, cfg.ChatService)

	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    cfg.ChatService.GRPCServer.Keepalive.Time,
			Timeout: cfg.ChatService.GRPCServer.Keepalive.Timeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             cfg.ChatService.GRPCServer.Keepalive.MinTime,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(
			middleware.UnaryRecoveryInterceptor(),
			middleware.UnaryLoggingInterceptor(),
			telemetry.MetricsServerInterceptor("chat-service"),
			middleware.TimeoutServerInterceptor(cfg.ChatService.GRPCServer.Timeout),
		),
	)
	pb.RegisterChatServiceServer(grpcServer, chatService)

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("chat.v1.ChatService", grpc_health_v1.HealthCheckResponse_SERVING)

	reflection.Register(grpcServer)

	return runServer(ctx, cfg, grpcServer)
}

func loadConfig() (*chat.Config, error) {
	env := config.GetEnv()
	logger.InitLogger(env)

	return config.Load[chat.Config]("configs", "base", env)
}

func runServer(ctx context.Context, cfg *chat.Config, grpcServer *grpc.Server) error {
	lis, err := net.Listen("tcp", ":"+cfg.Port.ChatGRPC)
	if err != nil {
		return err
	}

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		slog.InfoContext(ctx, "Starting Chat Service", "port", cfg.Port.ChatGRPC, "env", cfg.Env)
		return grpcServer.Serve(lis)
	})

	eg.Go(func() error {
		<-ctx.Done()
		slog.InfoContext(ctx, "Shutting down Chat Service...")
		grpcServer.GracefulStop()
		slog.InfoContext(ctx, "Chat Service stopped gracefully")
		return nil
	})

	return eg.Wait()
}
