package apigateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/shared/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRouter_HandleRefreshToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cookieValue  string
		hasCookie    bool
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:        "Success: 유효한 리프레시 토큰으로 갱신 성공",
			hasCookie:   true,
			cookieValue: "valid-token",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().RefreshToken(mock.Anything, &userpb.RefreshTokenRequest{
					RefreshToken: "valid-token",
				}).Return(&userpb.RefreshTokenResponse{
					AccessToken:  "new-access-token",
					RefreshToken: "new-refresh-token",
				}, nil)
			},
			expectedCode: http.StatusOK,
		},
		{
			name:         "Failure: 리프레시 토큰 쿠키 누락",
			hasCookie:    false,
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:        "Failure: 서비스 레이어에서 인증 실패",
			hasCookie:   true,
			cookieValue: "invalid-token",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().RefreshToken(mock.Anything, mock.Anything).
					Return(nil, errors.New("unauthenticated"))
			},
			expectedCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockUserClient := mocks.NewMockUserServiceClient(t)
			tt.mockBehavior(mockUserClient)

			r := &Router{
				userClient: mockUserClient,
				config: &Config{
					AppConfig: config.AppConfig{Env: "test"},
					UserService: config.UserConfig{
						Token: config.TokenConfig{
							RefreshTokenExpirationDays: 7,
						},
					},
				},
			}
			handler := http.HandlerFunc(r.handleRefreshToken)

			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/auth/token/refresh", nil)
			if tt.hasCookie {
				req.AddCookie(&http.Cookie{Name: "refresh_token", Value: tt.cookieValue})
			}

			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleRevokeToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		hasCookie    bool
		cookieValue  string
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:        "Success: 로그아웃 성공 및 쿠키 제거",
			hasCookie:   true,
			cookieValue: "valid-token",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().RevokeToken(mock.Anything, &userpb.RevokeTokenRequest{
					RefreshToken: "valid-token",
				}).Return(&userpb.RevokeTokenResponse{}, nil)
			},
			expectedCode: http.StatusNoContent,
		},
		{
			name:         "Failure: 로그아웃 시 리프레시 토큰 쿠키 누락",
			hasCookie:    false,
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockUserClient := mocks.NewMockUserServiceClient(t)
			tt.mockBehavior(mockUserClient)

			r := &Router{
				userClient: mockUserClient,
				config:     &Config{AppConfig: config.AppConfig{Env: "test"}},
			}
			handler := http.HandlerFunc(r.handleRevokeToken)

			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/auth/logout", nil)
			if tt.hasCookie {
				req.AddCookie(&http.Cookie{Name: "refresh_token", Value: tt.cookieValue})
			}

			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)

			if tt.expectedCode == http.StatusNoContent {
				var tokenCookie *http.Cookie
				for _, c := range w.Result().Cookies() {
					if c.Name == "refresh_token" {
						tokenCookie = c
						break
					}
				}
				assert.NotNil(t, tokenCookie)
				assert.Equal(t, -1, tokenCookie.MaxAge)
			}
		})
	}
}

func TestRouter_HandleGRPCError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		expectedCode int
	}{
		{
			name:         "Success: NotFound 에러 매핑",
			err:          status.Error(codes.NotFound, "not found"),
			expectedCode: http.StatusNotFound,
		},
		{
			name:         "Success: AlreadyExists 에러 매핑",
			err:          status.Error(codes.AlreadyExists, "already exists"),
			expectedCode: http.StatusConflict,
		},
		{
			name:         "Success: InvalidArgument 에러 매핑",
			err:          status.Error(codes.InvalidArgument, "invalid"),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Success: Unauthenticated 에러 매핑",
			err:          status.Error(codes.Unauthenticated, "unauth"),
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Success: PermissionDenied 에러 매핑",
			err:          status.Error(codes.PermissionDenied, "denied"),
			expectedCode: http.StatusForbidden,
		},
		{
			name:         "Success: FailedPrecondition 에러 매핑",
			err:          status.Error(codes.FailedPrecondition, "precondition"),
			expectedCode: http.StatusConflict,
		},
		{
			name:         "Success: ResourceExhausted 에러 매핑",
			err:          status.Error(codes.ResourceExhausted, "exhausted"),
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name:         "Success: DeadlineExceeded 에러 매핑",
			err:          status.Error(codes.DeadlineExceeded, "deadline"),
			expectedCode: http.StatusGatewayTimeout,
		},
		{
			name:         "Success: Unknown 에러 매핑",
			err:          status.Error(codes.Unknown, "unknown error"),
			expectedCode: http.StatusInternalServerError,
		},
		{
			name:         "Success: 일반 에러(Non-gRPC) 매핑",
			err:          errors.New("standard error"),
			expectedCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			writeProblemFromGRPC(w, req, tt.err)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}
