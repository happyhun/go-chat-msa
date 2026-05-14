package wsgateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sync/atomic"

	"go-chat-msa/internal/shared/auth"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/membership"
	"go-chat-msa/internal/wsgateway/loadbalance"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRingRefresher struct {
	calls    atomic.Int32
	observed bool
}

func (f *fakeRingRefresher) ForceReconcile() {
	f.calls.Add(1)
}

func (f *fakeRingRefresher) HasObservedMembers() bool {
	return f.observed
}

const testInternalSecret = "test-internal-secret"

func testConfig() *Config {
	return &Config{
		WSGateway: WSGatewayConfig{
			Server: config.HTTPWSServerConfig{
				ReadHeaderTimeout: 10 * time.Second,
			},
			HTTPClient: config.HTTPClientConfig{
				Timeout:             3 * time.Second,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
			},
			TicketTTL: 30 * time.Second,
			RateLimit: RateLimitConfig{
				Public:      config.RateLimitConfig{RPS: 1000, Burst: 1000, TTL: time.Hour},
				WSEstablish: config.RateLimitConfig{RPS: 1000, Burst: 1000, TTL: time.Hour},
			},
		},
		Internal: config.InternalConfig{
			Secret: testInternalSecret,
		},
		JWT: config.JWTConfig{
			Secret: "test-jwt-secret",
		},
	}
}

func testRouter(t *testing.T, hashRing *loadbalance.HashRing) *Router {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	watcher := membership.NewWatcher(client, "wss:member:", hashRing)
	return NewRouter(testConfig(), hashRing, watcher, client)
}

func testRouterWithRefresher(t *testing.T, hashRing *loadbalance.HashRing, refresher RingRefresher) *Router {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRouter(testConfig(), hashRing, refresher, client)
}

func TestRouter_HandleInternalBroadcast(t *testing.T) {
	t.Parallel()
	r := testRouter(t, loadbalance.New([]string{"node1", "node2"}))

	tests := []struct {
		name         string
		roomID       string
		headers      map[string]string
		expectedCode int
	}{
		{
			name:         "Failure: 내부 비밀키 헤더 누락",
			roomID:       "test-room",
			headers:      map[string]string{},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Failure: 잘못된 내부 비밀키 헤더",
			roomID:       "test-room",
			headers:      map[string]string{"X-Internal-Secret": "wrong-secret"},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Success: 정상적인 요청이나 대상 노드 연결 불가 (Bad Gateway 기대)",
			roomID:       "test-room",
			headers:      map[string]string{"X-Internal-Secret": testInternalSecret},
			expectedCode: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("POST", "/internal/rooms/"+tt.roomID+"/broadcast", bytes.NewBufferString("{}"))
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleWSTicket(t *testing.T) {
	t.Parallel()
	r := testRouter(t, loadbalance.New([]string{"node1"}))

	tests := []struct {
		name         string
		setupReq     func() *http.Request
		expectedCode int
		checkBody    bool
	}{
		{
			name: "Failure: 티켓 요청 시 JWT 토큰 누락",
			setupReq: func() *http.Request {
				return httptest.NewRequest("POST", "/ws/ticket", nil)
			},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name: "Success: 유효한 토큰으로 웹소켓 티켓 발급",
			setupReq: func() *http.Request {
				at, _ := auth.GenerateJWT("user-123", "testuser", "test-jwt-secret", 15*time.Minute)
				req := httptest.NewRequest("POST", "/ws/ticket", nil)
				req.Header.Set("Authorization", "Bearer "+at)
				return req
			},
			expectedCode: http.StatusOK,
			checkBody:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, tt.setupReq())

			assert.Equal(t, tt.expectedCode, w.Code)
			if tt.checkBody {
				var res TicketResponse
				err := json.Unmarshal(w.Body.Bytes(), &res)
				require.NoError(t, err)
				assert.NotEmpty(t, res.Ticket)
			}
		})
	}
}

func TestRouter_ProxyWebSocket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		ticket       string
		roomID       string
		setup        func(t *testing.T, r *Router)
		expectedCode int
	}{
		{
			name:         "Failure: 웹소켓 요청 시 티켓 누락",
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Failure: 잘못된 웹소켓 티켓 제공",
			ticket:       "invalid-ticket",
			expectedCode: http.StatusUnauthorized,
		},
		{
			name: "Failure: 프록시 요청 시 룸 ID 누락",
			setup: func(t *testing.T, r *Router) {
				require.NoError(t, r.ticketStore.Set(t.Context(), "temp-ticket-456", "user-456", time.Minute))
			},
			ticket:       "temp-ticket-456",
			expectedCode: http.StatusBadRequest,
		},
		{
			name: "Failure: 룸 페어링을 위한 노드를 찾을 수 없음 (부팅 race, ring 미초기화)",
			setup: func(t *testing.T, r *Router) {
				require.NoError(t, r.ticketStore.Set(t.Context(), "ticket-no-node", "user-123", time.Minute))
				r.hashRing = loadbalance.New([]string{})
			},
			ticket:       "ticket-no-node",
			roomID:       "test-room",
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name: "Success: 웹소켓 연결 프록시 시도",
			setup: func(t *testing.T, r *Router) {
				require.NoError(t, r.ticketStore.Set(t.Context(), "valid-ticket-123", "user-123", time.Minute))
			},
			ticket:       "valid-ticket-123",
			roomID:       "test-room",
			expectedCode: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := testRouter(t, loadbalance.New([]string{"node1"}))
			if tt.setup != nil {
				tt.setup(t, r)
			}

			url := "/ws"
			query := []string{}
			if tt.ticket != "" {
				query = append(query, "ticket="+tt.ticket)
			}
			if tt.roomID != "" {
				query = append(query, "room_id="+tt.roomID)
			}
			if len(query) > 0 {
				url += "?" + strings.Join(query, "&")
			}

			req := httptest.NewRequest("GET", url, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleProxyRoomRequest(t *testing.T) {
	t.Parallel()
	r := testRouter(t, loadbalance.New([]string{"node1"}))

	tests := []struct {
		name         string
		headerName   string
		headerVal    string
		expectedCode int
	}{
		{
			name:         "Failure: 룸 프록시 요청 시 비밀키 헤더 누락",
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Success: 정상 헤더이나 룸 연결 실패",
			headerName:   "X-Internal-Secret",
			headerVal:    testInternalSecret,
			expectedCode: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("DELETE", "/internal/rooms/room-1", nil)
			if tt.headerName != "" {
				req.Header.Set(tt.headerName, tt.headerVal)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_MisdirectedConvertedToServiceUnavailable(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpio.WriteProblem(r.Context(), w, http.StatusMisdirectedRequest, "not the owner of this room")
	}))
	t.Cleanup(backend.Close)

	backendAddr := strings.TrimPrefix(backend.URL, "http://")
	refresher := &fakeRingRefresher{}
	r := testRouterWithRefresher(t, loadbalance.New([]string{backendAddr}), refresher)

	proxy, ok := r.getOrCreateProxy(backendAddr)
	require.True(t, ok)

	req := httptest.NewRequest("GET", "/ws?room_id=room-1", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"wss의 421 응답을 503으로 변환")
	assert.Equal(t, "application/problem+json", w.Header().Get("Content-Type"),
		"problem+json content-type 유지")
	assert.Equal(t, int32(1), refresher.calls.Load(),
		"misdirect 응답에서 ForceReconcile이 호출되어야 함")

	var problem httpio.ProblemDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem),
		"body가 problem+json 포맷이어야 함")
	assert.Equal(t, http.StatusServiceUnavailable, problem.Status,
		"body의 status도 503으로 일치해야 함 (backend의 421 body 잔재 방지)")
	assert.Equal(t, http.StatusText(http.StatusServiceUnavailable), problem.Title)
	assert.NotEmpty(t, problem.Detail)
}
