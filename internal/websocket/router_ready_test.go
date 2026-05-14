package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/wsgateway/loadbalance"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
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

func TestRouter_Ready(t *testing.T) {
	t.Parallel()

	const selfAddr = "self:8081"
	cfg := WebSocketConfig{
		AdvertisedAddr: selfAddr,
		Manager: config.ManagerConfig{
			WriteWait:   time.Second,
			PongWait:    time.Second,
			PingPeriod:  time.Second,
			IdleTimeout: time.Minute,
			MaxLength:   1000,
		},
		GRPCClient: config.GRPCClientConfig{Timeout: time.Second},
		RateLimit: RateLimitConfig{
			WSMessage: config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
		},
	}

	t.Run("Success: dependencies and self ring member ready", func(t *testing.T) {
		t.Parallel()
		mr := miniredis.RunT(t)
		redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })

		serving := fakeHealthClient{status: grpc_health_v1.HealthCheckResponse_SERVING}
		r := NewRouter(nil, nil, cfg, loadbalance.New([]string{selfAddr}),
			WithRedisClient(redisClient),
			WithHealthClients(serving, serving))

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("Failure: self not present in ring", func(t *testing.T) {
		t.Parallel()
		mr := miniredis.RunT(t)
		redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })

		serving := fakeHealthClient{status: grpc_health_v1.HealthCheckResponse_SERVING}
		r := NewRouter(nil, nil, cfg, loadbalance.New([]string{"other:8081"}),
			WithRedisClient(redisClient),
			WithHealthClients(serving, serving))

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
}
