package websocket

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var wsMeter = otel.Meter("go-chat-msa/websocket")

var (
	ownerRejectedTotal metric.Int64Counter
)

func init() {
	var err error
	ownerRejectedTotal, err = wsMeter.Int64Counter("gochat_websocket_owner_rejected",
		metric.WithDescription("self-check에서 owner 아닌 요청을 거절한 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_websocket_owner_rejected", "error", err)
	}
}
