package hub

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var hubMeter = otel.Meter("go-chat-msa/websocket/hub")

var (
	hubsActive                    metric.Int64UpDownCounter
	hubsClosedTotal               metric.Int64Counter
	connectionsActive             metric.Int64UpDownCounter
	sessionConflictsTotal         metric.Int64Counter
	messagesReceivedTotal         metric.Int64Counter
	messagesRateLimitedTotal      metric.Int64Counter
	messagesSentTotal             metric.Int64Counter
	duplicateMessagesDroppedTotal metric.Int64Counter
	sendQueueDroppedTotal         metric.Int64Counter
	broadcastChannelDepth         metric.Float64Histogram
	persistChannelDepth           metric.Float64Gauge
	persistDroppedTotal           metric.Int64Counter
	fanoutDuration                metric.Float64Histogram
	egressDuration                metric.Float64Histogram
)

func init() {
	var err error
	hubsActive, err = hubMeter.Int64UpDownCounter("gochat_ws_hubs_active",
		metric.WithDescription("활성 Hub 고루틴 수 (방당 1개)"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_hubs_active", "error", err)
	}
	hubsClosedTotal, err = hubMeter.Int64Counter("gochat_ws_hubs_closed",
		metric.WithDescription("종료된 Hub 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_hubs_closed", "error", err)
	}
	connectionsActive, err = hubMeter.Int64UpDownCounter("gochat_ws_connections_active",
		metric.WithDescription("활성 WebSocket 세션 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_connections_active", "error", err)
	}
	sessionConflictsTotal, err = hubMeter.Int64Counter("gochat_ws_session_conflicts",
		metric.WithDescription("중복 연결로 끊긴 세션 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_session_conflicts", "error", err)
	}
	messagesReceivedTotal, err = hubMeter.Int64Counter("gochat_ws_messages_received",
		metric.WithDescription("클라이언트로부터 수신한 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_messages_received", "error", err)
	}
	messagesRateLimitedTotal, err = hubMeter.Int64Counter("gochat_ws_messages_rate_limited",
		metric.WithDescription("속도 제한 초과로 폐기된 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_messages_rate_limited", "error", err)
	}
	messagesSentTotal, err = hubMeter.Int64Counter("gochat_ws_messages_sent",
		metric.WithDescription("클라이언트로 송신한 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_messages_sent", "error", err)
	}
	duplicateMessagesDroppedTotal, err = hubMeter.Int64Counter("gochat_ws_duplicate_messages_dropped",
		metric.WithDescription("멱등성 캐시에 의해 폐기된 중복 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_duplicate_messages_dropped", "error", err)
	}
	sendQueueDroppedTotal, err = hubMeter.Int64Counter("gochat_ws_send_queue_dropped",
		metric.WithDescription("송신 큐 포화로 폐기된 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_send_queue_dropped", "error", err)
	}
	broadcastChannelDepth, err = hubMeter.Float64Histogram("gochat_ws_broadcast_channel_depth",
		metric.WithDescription("메시지 디큐 시점의 브로드캐스트 채널 깊이"),
		metric.WithExplicitBucketBoundaries(0, 1, 5, 10, 25, 50, 100, 150, 200, 256),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_broadcast_channel_depth", "error", err)
	}
	persistChannelDepth, err = hubMeter.Float64Gauge("gochat_ws_persist_channel_depth",
		metric.WithDescription("영속화 채널 현재 깊이"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_persist_channel_depth", "error", err)
	}
	persistDroppedTotal, err = hubMeter.Int64Counter("gochat_ws_persist_dropped",
		metric.WithDescription("영속화 채널 포화로 폐기된 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_persist_dropped", "error", err)
	}
	fanoutDuration, err = hubMeter.Float64Histogram("gochat_ws_fanout_duration_seconds",
		metric.WithDescription("팬아웃 지연 시간 (수신 → 세션 큐 적재)"),
		metric.WithExplicitBucketBoundaries(.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_fanout_duration_seconds", "error", err)
	}
	egressDuration, err = hubMeter.Float64Histogram("gochat_ws_egress_duration_seconds",
		metric.WithDescription("송신 지연 시간 (수신 → 네트워크 전송)"),
		metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .1, .25, .5, 1.0, 2.5, 5.0),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_egress_duration_seconds", "error", err)
	}
}

func observeFanout(ctx context.Context, receivedAt time.Time) {
	if receivedAt.IsZero() {
		return
	}
	fanoutDuration.Record(ctx, time.Since(receivedAt).Seconds())
}

func observeEgress(ctx context.Context, receivedAt time.Time) {
	if receivedAt.IsZero() {
		return
	}
	egressDuration.Record(ctx, time.Since(receivedAt).Seconds())
}
