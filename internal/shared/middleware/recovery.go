package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	recoveryMeter       = otel.Meter("go-chat-msa/middleware/recovery")
	panicRecoveredTotal metric.Int64Counter
)

func init() {
	var err error
	panicRecoveredTotal, err = recoveryMeter.Int64Counter("gochat_panic_recovered",
		metric.WithDescription("복구된 패닉 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_panic_recovered", "error", err)
	}
}

func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				panicRecoveredTotal.Add(r.Context(), 1)
				slog.ErrorContext(r.Context(), "panic recovered",
					"error", v,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
				)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func UnaryRecoveryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		defer func() {
			if v := recover(); v != nil {
				panicRecoveredTotal.Add(context.Background(), 1)
				slog.ErrorContext(ctx, "panic recovered",
					"error", v,
					"stack", string(debug.Stack()),
					"method", info.FullMethod,
				)
				err = status.Errorf(codes.Internal, "internal error: %v", fmt.Sprint(v))
			}
		}()
		return handler(ctx, req)
	}
}
