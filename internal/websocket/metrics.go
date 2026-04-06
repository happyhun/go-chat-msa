package websocket

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var wsMeter = otel.Meter("go-chat-msa/websocket")

var (
	persistenceBatchSaveTotal      metric.Int64Counter
	persistenceRetryQueueDepth     metric.Float64Gauge
	persistenceRetryOldestAge      metric.Float64Gauge
	persistenceRetrySaveTotal      metric.Int64Counter
	persistenceRetryQueueFullTotal metric.Int64Counter
)

func init() {
	var err error
	persistenceBatchSaveTotal, err = wsMeter.Int64Counter("gochat_persistence_batch_save",
		metric.WithDescription("배치 저장 시도 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_persistence_batch_save", "error", err)
	}
	persistenceRetryQueueDepth, err = wsMeter.Float64Gauge("gochat_persistence_retry_queue_depth",
		metric.WithDescription("재시도 큐 대기 배치 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_persistence_retry_queue_depth", "error", err)
	}
	persistenceRetryOldestAge, err = wsMeter.Float64Gauge("gochat_persistence_retry_oldest_age_seconds",
		metric.WithDescription("재시도 대기 중 가장 오래된 작업의 경과 시간(초)"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_persistence_retry_oldest_age_seconds", "error", err)
	}
	persistenceRetrySaveTotal, err = wsMeter.Int64Counter("gochat_persistence_retry_save",
		metric.WithDescription("재시도 저장 결과"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_persistence_retry_save", "error", err)
	}
	persistenceRetryQueueFullTotal, err = wsMeter.Int64Counter("gochat_persistence_retry_queue_full",
		metric.WithDescription("재시도 큐 포화로 폐기된 배치 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_persistence_retry_queue_full", "error", err)
	}
}
