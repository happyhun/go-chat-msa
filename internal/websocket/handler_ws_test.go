package websocket

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/shared/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testRoomID = "8a1f0c52-3d4e-4b6a-9c7d-1e2f3a4b5c6d"

func TestRouter_ServeWebSocket(t *testing.T) {
	t.Parallel()

	type mockBehavior func(m *mocks.MockUserServiceClient)

	tests := []struct {
		name         string
		queryParams  string
		userID       string
		mockBehavior mockBehavior
		expectedCode int
		expectedBody string
		disconnected bool
	}{
		{
			name:         "Failure: 티켓 누락",
			queryParams:  "?room_id=" + testRoomID,
			userID:       "",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
			expectedBody: "missing ticket",
		},
		{
			name:         "Failure: 존재하지 않는 티켓",
			queryParams:  "?room_id=" + testRoomID + "&ticket=unknown-ticket",
			userID:       "",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
			expectedBody: "invalid or expired ticket",
		},
		{
			name:         "Failure: 룸 ID 쿼리 파라미터 누락",
			queryParams:  "?",
			userID:       "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
			expectedBody: "missing room_id query parameter",
		},
		{
			name:         "Failure: 룸 ID가 UUID 형식이 아님",
			queryParams:  "?room_id=room-1",
			userID:       "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
			expectedBody: "room_id must be a canonical uuid",
		},
		{
			name:        "Failure: 채팅방 멤버가 아닌 경우 (Forbidden)",
			queryParams: "?room_id=" + testRoomID,
			userID:      "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyRoomMember(mock.Anything, &userpb.VerifyRoomMemberRequest{
					RoomId: testRoomID,
					UserId: "user-1",
				}).Return(nil, status.Error(codes.NotFound, "not a member of the room"))
			},
			expectedCode: http.StatusForbidden,
			expectedBody: "not a member of the room",
		},
		{
			name:        "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			queryParams: "?room_id=" + testRoomID,
			userID:      "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyRoomMember(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Internal, "service unavailable"))
			},
			expectedCode: http.StatusInternalServerError,
			expectedBody: "failed to verify room membership",
		},
		{
			name:        "Failure: NATS 연결 끊김",
			queryParams: "?room_id=" + testRoomID,
			userID:      "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyRoomMember(mock.Anything, mock.Anything).Return(&userpb.VerifyRoomMemberResponse{}, nil)
			},
			expectedCode: http.StatusServiceUnavailable,
			expectedBody: "room temporarily unavailable",
			disconnected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockUserClient := mocks.NewMockUserServiceClient(t)
			tt.mockBehavior(mockUserClient)

			redisClient := newTestRedis(t)
			router := NewRouter(mockUserClient, createTestConfig(), &fakeBus{connected: !tt.disconnected},
				WithPodName("test-pod"), WithRedisClient(redisClient))

			url := "/ws" + tt.queryParams
			if tt.userID != "" {
				url += "&ticket=" + issueTicket(t, redisClient, tt.userID)
			}
			req, err := http.NewRequest("GET", url, nil)
			require.NoError(t, err)

			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
			if tt.expectedBody != "" {
				assert.Contains(t, w.Body.String(), tt.expectedBody)
			}
		})
	}
}

func TestRouter_ServeWebSocket_ConnectRateLimit(t *testing.T) {
	t.Parallel()

	cfg := createTestConfig()
	cfg.RateLimit.WSConnect = config.RateLimitConfig{RPS: 1, Burst: 1, TTL: time.Minute}
	router := NewRouter(mocks.NewMockUserServiceClient(t), cfg, &fakeBus{connected: true},
		WithPodName("test-pod"), WithRedisClient(newTestRedis(t)))

	statuses := make([]int, 0, 2)
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/ws?room_id="+testRoomID, nil)
		req.Header.Set("X-Forwarded-For", "10.0.0.1")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		statuses = append(statuses, w.Code)
	}

	assert.Equal(t, []int{http.StatusUnauthorized, http.StatusTooManyRequests}, statuses,
		"same IP must be limited before the ticket is checked")
}
