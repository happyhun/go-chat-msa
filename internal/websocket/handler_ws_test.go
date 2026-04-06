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

func TestRouter_ServeHTTP(t *testing.T) {
	t.Parallel()

	type mockBehavior func(m *mocks.MockUserServiceClient)

	tests := []struct {
		name         string
		queryParams  string
		userID       string
		mockBehavior mockBehavior
		expectedCode int
		expectedBody string
	}{
		{
			name:         "Failure: 유저 식별 헤더(X-User-ID) 누락",
			queryParams:  "?room_id=room-1",
			userID:       "",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
			expectedBody: "missing X-User-ID header",
		},
		{
			name:         "Failure: 룸 ID 쿼리 파라미터 누락",
			queryParams:  "",
			userID:       "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
			expectedBody: "missing room_id query parameter",
		},
		{
			name:        "Failure: 채팅방 멤버가 아닌 경우 (Forbidden)",
			queryParams: "?room_id=room-1",
			userID:      "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyRoomMember(mock.Anything, &userpb.VerifyRoomMemberRequest{
					RoomId: "room-1",
					UserId: "user-1",
				}).Return(nil, status.Error(codes.NotFound, "not a member of the room"))
			},
			expectedCode: http.StatusForbidden,
			expectedBody: "not a member of the room",
		},
		{
			name:        "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			queryParams: "?room_id=room-1",
			userID:      "user-1",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyRoomMember(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Internal, "service unavailable"))
			},
			expectedCode: http.StatusInternalServerError,
			expectedBody: "failed to verify room membership",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockChatClient := mocks.NewMockChatServiceClient(t)
			mockUserClient := mocks.NewMockUserServiceClient(t)
			tt.mockBehavior(mockUserClient)

			router := NewRouter(mockChatClient, mockUserClient, createTestConfig())

			req, err := http.NewRequest("GET", "/ws"+tt.queryParams, nil)
			require.NoError(t, err)

			if tt.userID != "" {
				req.Header.Set("X-User-ID", tt.userID)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
			if tt.expectedBody != "" {
				assert.Contains(t, w.Body.String(), tt.expectedBody)
			}
		})
	}
}

func createTestConfig() WebSocketConfig {
	return WebSocketConfig{
		Manager: config.ManagerConfig{
			WriteWait:   10 * time.Second,
			PongWait:    60 * time.Second,
			PingPeriod:  54 * time.Second,
			IdleTimeout: 5 * time.Minute,
		},
	}
}
