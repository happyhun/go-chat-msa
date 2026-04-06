package wsgateway

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	wsgatewayMeter = otel.Meter("go-chat-msa/wsgateway")
	routedTotal    metric.Int64Counter
)

func init() {
	var err error
	routedTotal, err = wsgatewayMeter.Int64Counter("gochat_wsgateway_routed",
		metric.WithDescription("엔드포인트별 WebSocket 연결 라우팅 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_wsgateway_routed", "error", err)
	}
}
