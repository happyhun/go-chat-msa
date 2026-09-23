package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/chat"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/telemetry"

	"github.com/nats-io/nats.go"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.ErrorContext(context.Background(), "application failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger.InitLogger(cfg.Env)

	if cfg.Telemetry.OTelEndpoint != "" {
		shutdown, err := telemetry.InitOTel(ctx, "chat-service", cfg.Telemetry.OTelEndpoint)
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
		stopProfiler, err := telemetry.InitProfiling("chat-service", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.DB.MongoURI).SetPoolMonitor(telemetry.NewMongoPoolMonitor()))
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

	nc, err := nats.Connect(cfg.NATSURL(), nats.Name(os.Getenv("POD_NAME")), nats.MaxReconnects(-1), nats.ReconnectBufSize(-1))
	if err != nil {
		return err
	}
	defer nc.Close()
	journal := true
	msgCol := mongoClient.Database("chat_service").Collection("messages", options.Collection().SetWriteConcern(&writeconcern.WriteConcern{W: 1, Journal: &journal}))
	repo := chat.NewRepository(telemetry.NewInstrumentedCollection(msgCol))
	setupCtx, cancelSetup := context.WithTimeout(ctx, 10*time.Second)
	persistence, err := chat.NewPersistence(setupCtx, nc, repo, cfg.Persistence, func(ctx context.Context) error { return mongoClient.Ping(ctx, readpref.Primary()) })
	cancelSetup()
	if err != nil {
		return err
	}
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
	setChatServingStatus(healthServer, grpc_health_v1.HealthCheckResponse_SERVING)
	go reportHealth(ctx, healthServer, persistence)

	reflection.Register(grpcServer)

	return runServer(ctx, cfg, grpcServer, healthServer, persistence)
}

func loadConfig() (*chat.Config, error) {
	return config.LoadRuntime[chat.Config]()
}

func runServer(ctx context.Context, cfg *chat.Config, grpcServer *grpc.Server, healthServer *health.Server, persistence *chat.Persistence) error {
	lis, err := net.Listen("tcp", ":"+cfg.Port.ChatGRPC)
	if err != nil {
		return err
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error { return persistence.Run(ctx) })

	eg.Go(func() error {
		slog.InfoContext(ctx, "Starting Chat Service", "port", cfg.Port.ChatGRPC, "env", cfg.Env)
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			return err
		}
		return nil
	})

	eg.Go(func() error {
		<-ctx.Done()
		slog.InfoContext(ctx, "Shutting down Chat Service...")
		setChatServingStatus(healthServer, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		stop := time.AfterFunc(cfg.ShutdownTimeout, grpcServer.Stop)
		defer stop.Stop()
		grpcServer.GracefulStop()
		slog.InfoContext(ctx, "Chat Service stopped gracefully")
		return nil
	})

	return eg.Wait()
}

func reportHealth(ctx context.Context, healthServer *health.Server, persistence *chat.Persistence) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	update := func() {
		status := grpc_health_v1.HealthCheckResponse_SERVING
		if !persistence.Connected() {
			status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
		}
		setChatServingStatus(healthServer, status)
		queryStatus := grpc_health_v1.HealthCheckResponse_NOT_SERVING
		if persistence.QueryReady() {
			queryStatus = grpc_health_v1.HealthCheckResponse_SERVING
		}
		healthServer.SetServingStatus("chat.v1.ChatQuery", queryStatus)
	}

	update()
	for {
		select {
		case <-ctx.Done():
			setChatServingStatus(healthServer, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
			healthServer.SetServingStatus("chat.v1.ChatQuery", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
			return
		case <-ticker.C:
			update()
		}
	}
}

func setChatServingStatus(healthServer *health.Server, status grpc_health_v1.HealthCheckResponse_ServingStatus) {
	healthServer.SetServingStatus("", status)
	healthServer.SetServingStatus("chat.v1.ChatService", status)
	healthServer.SetServingStatus("chat.v1.ChatCommand", status)
}
