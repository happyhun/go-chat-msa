package telemetry

import (
	"context"
	"log/slog"
	"runtime/pprof"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

var (
	grpcMeter           = otel.Meter("go-chat-msa/metrics/grpc")
	grpcRequestsTotal   metric.Int64Counter
	grpcRequestDuration metric.Float64Histogram
)

func init() {
	var err error
	grpcRequestsTotal, err = grpcMeter.Int64Counter("gochat_grpc_requests",
		metric.WithDescription("gRPC 서버 unary 호출 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_grpc_requests", "error", err)
	}
	grpcRequestDuration, err = grpcMeter.Float64Histogram("gochat_grpc_request_duration_seconds",
		metric.WithDescription("gRPC 서버 unary 호출 소요 시간(초)"),
		metric.WithExplicitBucketBoundaries(.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_grpc_request_duration_seconds", "error", err)
	}
}

func MetricsServerInterceptor(serviceName string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {

		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			return handler(ctx, req)
		}

		start := time.Now()

		svc, method := splitMethod(info.FullMethod)

		var (
			resp any
			err  error
		)
		pprof.Do(ctx, pprof.Labels(
			"service", serviceName,
			"rpc.service", svc,
			"rpc.method", method,
		), func(ctx context.Context) {
			resp, err = handler(ctx, req)
		})

		code := status.Code(err).String()
		attrs := metric.WithAttributes(
			attribute.String("service", serviceName),
			attribute.String("method", info.FullMethod),
			attribute.String("code", code),
		)
		grpcRequestsTotal.Add(ctx, 1, attrs)
		grpcRequestDuration.Record(ctx, time.Since(start).Seconds(), attrs)

		return resp, err
	}
}

func splitMethod(fullMethod string) (svc, method string) {
	parts := strings.Split(strings.TrimPrefix(fullMethod, "/"), "/")
	if len(parts) != 2 {
		return fullMethod, ""
	}

	svcParts := strings.Split(parts[0], ".")
	return svcParts[len(svcParts)-1], parts[1]
}
