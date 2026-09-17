package apigateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-chat-msa/internal/shared/auth"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/wsticket"

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

func newTestRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	return redisClient
}

func TestRouter_Ready(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		APIGateway: GatewayConfig{
			HTTPClient: config.HTTPClientConfig{
				Timeout:             time.Second,
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
			},
			RateLimit: RateLimitConfig{
				Public:        config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
				Authenticated: config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
			},
		},
	}

	t.Run("Success: all dependencies ready", func(t *testing.T) {
		t.Parallel()

		serving := fakeHealthClient{status: grpc_health_v1.HealthCheckResponse_SERVING}
		r := NewRouter(cfg, nil, nil, newTestRedisClient(t), WithHealthClients(serving, serving))

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("Failure: grpc dependency not serving", func(t *testing.T) {
		t.Parallel()

		serving := fakeHealthClient{status: grpc_health_v1.HealthCheckResponse_SERVING}
		notServing := fakeHealthClient{status: grpc_health_v1.HealthCheckResponse_NOT_SERVING}
		r := NewRouter(cfg, nil, nil, newTestRedisClient(t), WithHealthClients(notServing, serving))

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
}

func TestRouter_HandleCreateWSTicket(t *testing.T) {
	t.Parallel()

	const jwtSecret = "test-jwt-secret"
	cfg := &Config{
		JWT: config.JWTConfig{Secret: jwtSecret},
		APIGateway: GatewayConfig{
			HTTPClient: config.HTTPClientConfig{Timeout: time.Second, MaxIdleConns: 10, MaxIdleConnsPerHost: 10},
			TicketTTL:  30 * time.Second,
			RateLimit: RateLimitConfig{
				Public:        config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
				Authenticated: config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
				WSTicket:      config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
			},
		},
	}

	tests := []struct {
		name         string
		token        func(t *testing.T) string
		expectedCode int
	}{
		{
			name:         "Failure: JWT 토큰 누락",
			token:        func(*testing.T) string { return "" },
			expectedCode: http.StatusUnauthorized,
		},
		{
			name: "Success: 발급한 티켓을 같은 사용자로 소비할 수 있다",
			token: func(t *testing.T) string {
				token, err := auth.GenerateJWT("user-123", "testuser", jwtSecret, 15*time.Minute)
				require.NoError(t, err)
				return token
			},
			expectedCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			redisClient := newTestRedisClient(t)
			r := NewRouter(cfg, nil, nil, redisClient)

			req := httptest.NewRequest(http.MethodPost, "/auth/ws-ticket", nil)
			if token := tt.token(t); token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, tt.expectedCode, w.Code)
			if tt.expectedCode != http.StatusOK {
				return
			}
			var res WSTicketResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
			userID, ok, err := wsticket.NewStore(redisClient).Consume(t.Context(), res.Ticket)
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, "user-123", userID)
		})
	}
}
