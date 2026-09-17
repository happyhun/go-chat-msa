package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/wsticket"
	"go-chat-msa/internal/websocket/hub"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type fakeHealthClient struct {
	status grpc_health_v1.HealthCheckResponse_ServingStatus
	err    error
}

func (f fakeHealthClient) Check(context.Context, *grpc_health_v1.HealthCheckRequest, ...grpc.CallOption) (*grpc_health_v1.HealthCheckResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &grpc_health_v1.HealthCheckResponse{Status: f.status}, nil
}

func (f fakeHealthClient) List(context.Context, *grpc_health_v1.HealthListRequest, ...grpc.CallOption) (*grpc_health_v1.HealthListResponse, error) {
	return &grpc_health_v1.HealthListResponse{}, nil
}

func (f fakeHealthClient) Watch(context.Context, *grpc_health_v1.HealthCheckRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[grpc_health_v1.HealthCheckResponse], error) {
	return nil, nil
}

type fakeBus struct {
	connected  bool
	published  []hub.Envelope
	events     []hub.RoomEvent
	publishErr error
}

type fakeSubscription struct{}

func (fakeSubscription) Unsubscribe() error { return nil }

func (f *fakeBus) PublishMessage(_ context.Context, env hub.Envelope) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, env)
	return nil
}

func (f *fakeBus) SubscribeRoom(_ context.Context, _ string, _ func(hub.Envelope)) (hub.Subscription, error) {
	return fakeSubscription{}, nil
}

func (f *fakeBus) PublishEvent(_ context.Context, ev hub.RoomEvent) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeBus) SubscribeEvents(_ func(hub.RoomEvent)) (hub.Subscription, error) {
	return fakeSubscription{}, nil
}

func (f *fakeBus) Connected() bool { return f.connected }

func createTestConfig() WebSocketConfig {
	return WebSocketConfig{
		Manager: config.ManagerConfig{
			WriteWait:   10 * time.Second,
			PongWait:    60 * time.Second,
			PingPeriod:  54 * time.Second,
			IdleTimeout: 5 * time.Minute,
		},
		GRPCClient: config.GRPCClientConfig{Timeout: time.Second},
		RateLimit: RateLimitConfig{
			WSConnect: config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
		},
		NATS: NATSConfig{
			FlushTimeout:    2 * time.Second,
			MaxDeliveryLag:  2 * time.Second,
			SubPendingMsgs:  1000,
			SubPendingBytes: 8 * 1024 * 1024,
		},
	}
}

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func issueTicket(t *testing.T, client *redis.Client, userID string) string {
	t.Helper()
	ticket, err := wsticket.NewStore(client).Issue(t.Context(), userID, time.Minute)
	require.NoError(t, err)
	return ticket
}

func TestRouter_Ready(t *testing.T) {
	t.Parallel()

	serving := fakeHealthClient{status: grpc_health_v1.HealthCheckResponse_SERVING}

	tests := []struct {
		name         string
		bus          *fakeBus
		health       fakeHealthClient
		noRedis      bool
		expectedCode int
	}{
		{
			name:         "Success: Redis, NATS 연결과 의존 서비스가 정상",
			bus:          &fakeBus{connected: true},
			health:       serving,
			expectedCode: http.StatusOK,
		},
		{
			name:         "Failure: Redis 미설정",
			bus:          &fakeBus{connected: true},
			health:       serving,
			noRedis:      true,
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name:         "Failure: NATS 연결 끊김",
			bus:          &fakeBus{connected: false},
			health:       serving,
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name:         "Failure: user-service health 비정상",
			bus:          &fakeBus{connected: true},
			health:       fakeHealthClient{status: grpc_health_v1.HealthCheckResponse_NOT_SERVING},
			expectedCode: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := []RouterOption{WithPodName("test-pod"), WithHealthClients(tt.health)}
			if !tt.noRedis {
				opts = append(opts, WithRedisClient(newTestRedis(t)))
			}
			r := NewRouter(nil, createTestConfig(), tt.bus, opts...)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}
