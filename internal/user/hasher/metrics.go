package hasher

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	hasherMeter          = otel.Meter("go-chat-msa/user/hasher")
	hasherJobsTotal      metric.Int64Counter
	hasherDuration       metric.Float64Histogram
	hasherQueueDepth     metric.Float64Gauge
	hasherQueueFullTotal metric.Int64Counter
)

func init() {
	var err error
	hasherJobsTotal, err = hasherMeter.Int64Counter("gochat_hasher_jobs",
		metric.WithDescription("해싱 작업 처리 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_hasher_jobs", "error", err)
	}
	hasherDuration, err = hasherMeter.Float64Histogram("gochat_hasher_duration_seconds",
		metric.WithDescription("bcrypt 연산 소요 시간"),
		metric.WithExplicitBucketBoundaries(.05, .1, .25, .5, 1, 2, 4, 8),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_hasher_duration_seconds", "error", err)
	}
	hasherQueueDepth, err = hasherMeter.Float64Gauge("gochat_hasher_queue_depth",
		metric.WithDescription("해셔 큐 대기 작업 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_hasher_queue_depth", "error", err)
	}
	hasherQueueFullTotal, err = hasherMeter.Int64Counter("gochat_hasher_queue_full",
		metric.WithDescription("해셔 큐 포화로 거부된 작업 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_hasher_queue_full", "error", err)
	}
}

func jobTypeName(t jobType) string {
	if t == jobHash {
		return "hash"
	}
	return "compare"
}
