package membership

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var membershipMeter = otel.Meter("go-chat-msa/membership")

var membershipReconcileTotal metric.Int64Counter

func init() {
	var err error
	membershipReconcileTotal, err = membershipMeter.Int64Counter("gochat_membership_reconcile",
		metric.WithDescription("멤버십 reconcile 시도 결과"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_membership_reconcile", "error", err)
	}
}
