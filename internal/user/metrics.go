package user

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	userMeter           = otel.Meter("go-chat-msa/user")
	userCreatedTotal    metric.Int64Counter
	userDeletedTotal    metric.Int64Counter
	authLoginTotal      metric.Int64Counter
	authTokenReuseTotal metric.Int64Counter
	roomJoinTotal       metric.Int64Counter
)

func init() {
	var err error
	userCreatedTotal, err = userMeter.Int64Counter("gochat_user_created",
		metric.WithDescription("사용자 생성 시도 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_user_created", "error", err)
	}
	userDeletedTotal, err = userMeter.Int64Counter("gochat_user_deleted",
		metric.WithDescription("회원탈퇴 시도 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_user_deleted", "error", err)
	}
	authLoginTotal, err = userMeter.Int64Counter("gochat_auth_login",
		metric.WithDescription("로그인 시도 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_auth_login", "error", err)
	}
	authTokenReuseTotal, err = userMeter.Int64Counter("gochat_auth_token_reuse_detected",
		metric.WithDescription("리프레시 토큰 재사용 감지 횟수 (토큰 탈취 가능성)"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_auth_token_reuse_detected", "error", err)
	}
	roomJoinTotal, err = userMeter.Int64Counter("gochat_room_join",
		metric.WithDescription("채팅방 입장 시도 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_room_join", "error", err)
	}
}
