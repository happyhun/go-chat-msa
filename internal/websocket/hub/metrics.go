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
	hubsActive               metric.Int64UpDownCounter
	hubsClosedTotal          metric.Int64Counter
	connectionsActive        metric.Int64UpDownCounter
	sessionsClosedTotal      metric.Int64Counter
	messagesReceivedTotal    metric.Int64Counter
	messagesRateLimitedTotal metric.Int64Counter
	messagesSentTotal        metric.Int64Counter
	sendQueueOverflowsTotal  metric.Int64Counter
	broadcastChannelDepth    metric.Float64Histogram
	egressDuration           metric.Float64Histogram
	brokerHopDuration        metric.Float64Histogram
	jetStreamPublishDuration metric.Float64Histogram
	hubFanoutDuration        metric.Float64Histogram
	outOfOrderTotal          metric.Int64Counter
	reorderSpanSeconds       metric.Float64Histogram
	natsSlowConsumerTotal    metric.Int64Counter
	natsDroppedMessagesTotal metric.Int64Counter
	natsPublishFailedTotal   metric.Int64Counter
	natsDisconnectsTotal     metric.Int64Counter
	roomEventsIgnoredTotal   metric.Int64Counter
)

func init() {
	var err error
	hubsActive, err = hubMeter.Int64UpDownCounter("gochat_ws_hubs_active",
		metric.WithDescription("활성 Hub 고루틴 수 (방당 1개)"),
	)
	warnOnMetricError("gochat_ws_hubs_active", err)

	hubsClosedTotal, err = hubMeter.Int64Counter("gochat_ws_hubs_closed",
		metric.WithDescription("종료된 Hub 수"),
	)
	warnOnMetricError("gochat_ws_hubs_closed", err)

	connectionsActive, err = hubMeter.Int64UpDownCounter("gochat_ws_connections_active",
		metric.WithDescription("활성 WebSocket 세션 수"),
	)
	warnOnMetricError("gochat_ws_connections_active", err)

	sessionsClosedTotal, err = hubMeter.Int64Counter("gochat_ws_sessions_closed",
		metric.WithDescription("서버가 이유를 달아 닫은 세션 수"),
	)
	warnOnMetricError("gochat_ws_sessions_closed", err)

	messagesReceivedTotal, err = hubMeter.Int64Counter("gochat_ws_messages_received",
		metric.WithDescription("클라이언트로부터 수신한 메시지 수"),
	)
	warnOnMetricError("gochat_ws_messages_received", err)

	messagesRateLimitedTotal, err = hubMeter.Int64Counter("gochat_ws_messages_rate_limited",
		metric.WithDescription("속도 제한 초과로 폐기된 메시지 수"),
	)
	warnOnMetricError("gochat_ws_messages_rate_limited", err)

	messagesSentTotal, err = hubMeter.Int64Counter("gochat_ws_messages_sent",
		metric.WithDescription("클라이언트로 송신한 메시지 수"),
	)
	warnOnMetricError("gochat_ws_messages_sent", err)

	sendQueueOverflowsTotal, err = hubMeter.Int64Counter("gochat_ws_send_queue_overflows",
		metric.WithDescription("세션 전송 버퍼 포화로 연결을 종료한 횟수"),
	)
	warnOnMetricError("gochat_ws_send_queue_overflows", err)

	broadcastChannelDepth, err = hubMeter.Float64Histogram("gochat_ws_broadcast_channel_depth",
		metric.WithDescription("메시지 디큐 시점의 브로드캐스트 채널 깊이"),
		metric.WithExplicitBucketBoundaries(0, 1, 5, 10, 25, 50, 100, 150, 200, 256),
	)
	warnOnMetricError("gochat_ws_broadcast_channel_depth", err)

	egressDuration, err = hubMeter.Float64Histogram("gochat_ws_egress_duration_seconds",
		metric.WithDescription("송신 → NATS 왕복 → 발신자 소켓 쓰기. 발신 Pod 단일 시계"),
		metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .1, .25, .5, 1.0, 2.5, 5.0),
	)
	warnOnMetricError("gochat_ws_egress_duration_seconds", err)

	brokerHopDuration, err = hubMeter.Float64Histogram("gochat_ws_broker_hop_duration_seconds",
		metric.WithDescription("NATS 수락 시각 → 구독 콜백 도착"),
		metric.WithExplicitBucketBoundaries(.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5),
	)
	warnOnMetricError("gochat_ws_broker_hop_duration_seconds", err)

	jetStreamPublishDuration, err = hubMeter.Float64Histogram("gochat_ws_jetstream_publish_ack_duration_seconds",
		metric.WithDescription("JetStream publish 호출부터 PubAck까지의 시간"),
		metric.WithExplicitBucketBoundaries(.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1),
	)
	warnOnMetricError("gochat_ws_jetstream_publish_ack_duration_seconds", err)

	hubFanoutDuration, err = hubMeter.Float64Histogram("gochat_ws_hub_fanout_duration_seconds",
		metric.WithDescription("구독 콜백 도착 → 그 Hub의 전 세션 큐 적재 완료. 수신 Pod 단일 시계"),
		metric.WithExplicitBucketBoundaries(.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1),
	)
	warnOnMetricError("gochat_ws_hub_fanout_duration_seconds", err)

	outOfOrderTotal, err = hubMeter.Int64Counter("gochat_ws_out_of_order",
		metric.WithDescription("그 방에 마지막으로 전달한 id보다 작은 id가 도착한 횟수"),
	)
	warnOnMetricError("gochat_ws_out_of_order", err)

	reorderSpanSeconds, err = hubMeter.Float64Histogram("gochat_ws_reorder_span_seconds",
		metric.WithDescription("역전 폭. 두 UUIDv7 id의 시각 차이"),
		metric.WithExplicitBucketBoundaries(.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5),
	)
	warnOnMetricError("gochat_ws_reorder_span_seconds", err)

	natsSlowConsumerTotal, err = hubMeter.Int64Counter("gochat_ws_nats_slow_consumer",
		metric.WithDescription("구독 slow consumer 발생 횟수"),
	)
	warnOnMetricError("gochat_ws_nats_slow_consumer", err)

	natsDroppedMessagesTotal, err = hubMeter.Int64Counter("gochat_ws_nats_dropped_messages",
		metric.WithDescription("slow consumer로 NATS 클라이언트가 버린 메시지 수"),
	)
	warnOnMetricError("gochat_ws_nats_dropped_messages", err)

	natsPublishFailedTotal, err = hubMeter.Int64Counter("gochat_ws_nats_publish_failed",
		metric.WithDescription("NATS 발행 실패 수"),
	)
	warnOnMetricError("gochat_ws_nats_publish_failed", err)

	natsDisconnectsTotal, err = hubMeter.Int64Counter("gochat_ws_nats_disconnects",
		metric.WithDescription("NATS 연결 끊김 횟수"),
	)
	warnOnMetricError("gochat_ws_nats_disconnects", err)

	roomEventsIgnoredTotal, err = hubMeter.Int64Counter("gochat_ws_room_events_ignored",
		metric.WithDescription("무시한 제어 이벤트 수"),
	)
	warnOnMetricError("gochat_ws_room_events_ignored", err)
}

func warnOnMetricError(name string, err error) {
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", name, "error", err)
	}
}

func observeEgress(ctx context.Context, receivedAt time.Time) {
	if receivedAt.IsZero() {
		return
	}
	egressDuration.Record(ctx, time.Since(receivedAt).Seconds())
}
