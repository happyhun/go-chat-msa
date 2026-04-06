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

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/shared/logger"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/telemetry"
	"go-chat-msa/internal/user"
	"go-chat-msa/internal/user/db"
	"go-chat-msa/internal/user/hasher"

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
		shutdown, err := telemetry.InitOTel(ctx, "user-service", cfg.Telemetry.OTelEndpoint)
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

		stopProfiler, err := telemetry.InitProfiling("user-service", cfg.Telemetry.PyroscopeEndpoint)
		if err != nil {
			slog.WarnContext(ctx, "failed to initialize pyroscope profiler", "error", err)
		} else {
			defer stopProfiler()
		}
	}

	hasherPool := hasher.NewPool(hasher.DefaultPoolConfig())
	defer hasherPool.Close()

	pgPool, err := database.NewPostgres(cfg.DB.PostgresURL)
	if err != nil {
		return err
	}
	defer pgPool.Close()

	telemetry.RegisterPgxpoolMetrics(pgPool)

	dbQueries := db.New(telemetry.InstrumentedDBTX(pgPool))
	userService := user.NewService(dbQueries, cfg.UserService, cfg.JWT.Secret, hasherPool).
		WithRunInTx(func(ctx context.Context, fn func(db.Querier) error) error {
			tx, err := pgPool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)

			if err := fn(db.New(telemetry.InstrumentedDBTX(tx))); err != nil {
				return err
			}

			return tx.Commit(ctx)
		})
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    cfg.UserService.GRPCServer.Keepalive.Time,
			Timeout: cfg.UserService.GRPCServer.Keepalive.Timeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             cfg.UserService.GRPCServer.Keepalive.MinTime,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(
			middleware.UnaryRecoveryInterceptor(),
			middleware.UnaryLoggingInterceptor(),
			telemetry.MetricsServerInterceptor("user-service"),
			middleware.TimeoutServerInterceptor(cfg.UserService.GRPCServer.Timeout),
		),
	)
	userpb.RegisterUserServiceServer(grpcServer, userService)

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("user.v1.UserService", grpc_health_v1.HealthCheckResponse_SERVING)

	reflection.Register(grpcServer)

	return runServer(ctx, cfg, grpcServer, userService)
}

func loadConfig() (*user.Config, error) {
	env := config.GetEnv()
	logger.InitLogger(env)

	return config.Load[user.Config]("configs", "base", env)
}

func runServer(ctx context.Context, cfg *user.Config, grpcServer *grpc.Server, userService *user.Service) error {
	lis, err := net.Listen("tcp", ":"+cfg.Port.UserGRPC)
	if err != nil {
		return err
	}

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		slog.InfoContext(ctx, "Starting User Service", "port", cfg.Port.UserGRPC, "env", cfg.Env)
		return grpcServer.Serve(lis)
	})

	eg.Go(func() error {
		userService.PurgeExpiredTokens(ctx)
		return nil
	})

	eg.Go(func() error {
		<-ctx.Done()
		slog.InfoContext(ctx, "Shutting down User Service...")
		grpcServer.GracefulStop()
		slog.InfoContext(ctx, "User Service stopped gracefully")
		return nil
	})

	return eg.Wait()
}
