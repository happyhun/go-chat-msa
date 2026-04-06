package middleware

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func UnaryLoggingInterceptor() grpc.UnaryServerInterceptor {
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
		resp, err := handler(ctx, req)
		latencyMs := time.Since(start).Milliseconds()

		code := status.Code(err)
		logLevel := slog.LevelInfo
		if isServerError(code) {
			logLevel = slog.LevelError
		}

		attrs := []slog.Attr{
			slog.String("method", info.FullMethod),
			slog.String("code", code.String()),
			slog.Int64("latency_ms", latencyMs),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}

		slog.LogAttrs(ctx, logLevel, "grpc request", attrs...)
		return resp, err
	}
}

func isServerError(c codes.Code) bool {
	switch c {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable:
		return true
	default:
		return false
	}
}
