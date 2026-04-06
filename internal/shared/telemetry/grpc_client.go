package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

var (
	grpcClientMeter           = otel.Meter("go-chat-msa/metrics/grpc-client")
	grpcClientRequestsTotal   metric.Int64Counter
	grpcClientRequestDuration metric.Float64Histogram
)

func init() {
	var err error
	grpcClientRequestsTotal, err = grpcClientMeter.Int64Counter("gochat_grpc_client_requests",
		metric.WithDescription("gRPC 클라이언트 unary 호출 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_grpc_client_requests", "error", err)
	}
	grpcClientRequestDuration, err = grpcClientMeter.Float64Histogram("gochat_grpc_client_request_duration_seconds",
		metric.WithDescription("gRPC 클라이언트 unary 호출 소요 시간(초)"),
		metric.WithExplicitBucketBoundaries(.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_grpc_client_request_duration_seconds", "error", err)
	}
}

func MetricsClientInterceptor(serviceName string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		if strings.HasPrefix(method, "/grpc.health.v1.Health/") {
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		code := status.Code(err).String()

		attrs := metric.WithAttributes(
			attribute.String("service", serviceName),
			attribute.String("method", method),
			attribute.String("code", code),
		)
		grpcClientRequestsTotal.Add(ctx, 1, attrs)
		grpcClientRequestDuration.Record(ctx, time.Since(start).Seconds(), attrs)

		return err
	}
}
