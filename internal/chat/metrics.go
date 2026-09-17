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
	chatPersistenceLag         metric.Float64Histogram
	chatBatchSize              metric.Float64Histogram
	chatRetryTotal             metric.Int64Counter
	chatDLQTotal               metric.Int64Counter
	chatPending                metric.Int64Gauge
	chatCircuit                metric.Int64Gauge
	chatWorkers                metric.Int64Gauge
	chatOldestPending          metric.Float64Gauge
)

func warnOnMetricError(name string, err error) {
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", name, "error", err)
	}
}

func init() {
	var err error
	chatWorkers, err = chatMeter.Int64Gauge("gochat_chat_persistence_workers")
	warnOnMetricError("gochat_chat_persistence_workers", err)
	chatOldestPending, err = chatMeter.Float64Gauge("gochat_chat_persistence_oldest_pending_age", metric.WithUnit("s"))
	warnOnMetricError("gochat_chat_persistence_oldest_pending_age", err)
	chatPersistenceLag, err = chatMeter.Float64Histogram("gochat_chat_persistence_lag", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(.01, .05, .1, .2, .5, 1, 5, 30, 60, 300))
	warnOnMetricError("gochat_chat_persistence_lag", err)
	chatBatchSize, err = chatMeter.Float64Histogram("gochat_chat_persistence_batch_size", metric.WithExplicitBucketBoundaries(1, 10, 50, 100, 250, 500))
	warnOnMetricError("gochat_chat_persistence_batch_size", err)
	chatRetryTotal, err = chatMeter.Int64Counter("gochat_chat_persistence_retry_total")
	warnOnMetricError("gochat_chat_persistence_retry_total", err)
	chatDLQTotal, err = chatMeter.Int64Counter("gochat_chat_persistence_dlq_total")
	warnOnMetricError("gochat_chat_persistence_dlq_total", err)
	chatPending, err = chatMeter.Int64Gauge("gochat_chat_persistence_pending")
	warnOnMetricError("gochat_chat_persistence_pending", err)
	chatCircuit, err = chatMeter.Int64Gauge("gochat_chat_persistence_circuit")
	warnOnMetricError("gochat_chat_persistence_circuit", err)
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
