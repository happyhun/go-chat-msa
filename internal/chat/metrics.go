package chat

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	chatMeter                  = otel.Meter("go-chat-msa/chat")
	chatMessagesSavedTotal     metric.Int64Counter
	chatHistoryFetchedMessages metric.Float64Histogram
)

func init() {
	var err error
	chatMessagesSavedTotal, err = chatMeter.Int64Counter("gochat_chat_messages_saved",
		metric.WithDescription("DB에 저장된 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_chat_messages_saved", "error", err)
	}
	chatHistoryFetchedMessages, err = chatMeter.Float64Histogram("gochat_chat_history_fetched_messages",
		metric.WithDescription("메시지 조회 시 반환된 건수 분포"),
		metric.WithExplicitBucketBoundaries(0, 10, 25, 50, 100, 200, 500),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_chat_history_fetched_messages", "error", err)
	}
}
