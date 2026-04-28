package apigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"


	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/middleware"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRouter_HandleCreateUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		body         any
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
		expectedBody any
	}{
		{
			name: "Success: 유효한 회원가입 요청",
			body: CreateUserRequest{
				Username: "testuser",
				Password: "SecurePass123!",
			},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().CreateUser(mock.Anything, &userpb.CreateUserRequest{
					Username: "testuser",
					Password: "SecurePass123!",
				}).Return(&userpb.CreateUserResponse{
					UserId: "new-user-id",
				}, nil)
			},
			expectedCode: http.StatusCreated,
			expectedBody: map[string]any{"user_id": "new-user-id"},
		},
		{
			name:         "Failure: 잘못된 JSON 본문 요청",
			body:         "invalid-json",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
			expectedBody: map[string]any{"detail": "invalid request body"},
		},
		{
			name: "Failure: 유저 서비스 내부 에러 (Internal)",
			body: CreateUserRequest{
				Username: "testuser",
				Password: "SecurePass123!",
			},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().CreateUser(mock.Anything, mock.Anything).
					Return(nil, errors.New("service error"))
			},
			expectedCode: http.StatusInternalServerError,
			expectedBody: nil,
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

			handler := http.HandlerFunc(r.handleCreateUser)

			var reqBody []byte
			if s, ok := tt.body.(string); ok {
				reqBody = []byte(s)
			} else {
				var err error
				reqBody, err = json.Marshal(tt.body)
				require.NoError(t, err)
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/users", bytes.NewBuffer(reqBody))
			req.Header.Set("Content-Type", "application/json")

			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)

			if tt.expectedBody != nil {
				var gotBody map[string]any
				err := json.Unmarshal(w.Body.Bytes(), &gotBody)
				require.NoError(t, err)

				if expectedMap, ok := tt.expectedBody.(map[string]any); ok {
					for k, v := range expectedMap {
						assert.Equal(t, v, gotBody[k])
					}
				}
			}
		})
	}
}

func TestRouter_HandleVerifyUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		body           any
		mockBehavior   func(m *mocks.MockUserServiceClient)
		expectedCode   int
		expectCookie   bool
		expectedCookie string
	}{
		{
			name: "Success: 액세스 토큰만 발급하는 로그인",
			body: VerifyUserRequest{
				Username: "testuser",
				Password: "SecurePass123!",
			},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyUser(mock.Anything, &userpb.VerifyUserRequest{
					Username: "testuser",
					Password: "SecurePass123!",
				}).Return(&userpb.VerifyUserResponse{
					UserId:      "user-id",
					AccessToken: "access-token",
				}, nil)
			},
			expectedCode: http.StatusOK,
			expectCookie: false,
		},
		{
			name: "Success: 리프레시 토큰 쿠키가 포함된 로그인",
			body: VerifyUserRequest{
				Username: "testuser",
				Password: "SecurePass123!",
			},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyUser(mock.Anything, mock.Anything).
					Return(&userpb.VerifyUserResponse{
						UserId:       "user-id",
						AccessToken:  "access-token",
						RefreshToken: "refresh-token",
					}, nil)
			},
			expectedCode:   http.StatusOK,
			expectCookie:   true,
			expectedCookie: "refresh-token",
		},
		{
			name: "Failure: 유저 서비스 에러 (잘못된 자격 증명 등)",
			body: VerifyUserRequest{
				Username: "testuser",
				Password: "SecurePass123!",
			},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().VerifyUser(mock.Anything, mock.Anything).
					Return(nil, errors.New("invalid credentials"))
			},
			expectedCode: http.StatusInternalServerError,
			expectCookie: false,
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

			handler := http.HandlerFunc(r.handleVerifyUser)

			var reqBody []byte
			if s, ok := tt.body.(string); ok {
				reqBody = []byte(s)
			} else {
				var err error
				reqBody, err = json.Marshal(tt.body)
				require.NoError(t, err)
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/auth/token", bytes.NewBuffer(reqBody))
			req.Header.Set("Content-Type", "application/json")

			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)

			if tt.expectCookie {
				var tokenCookie *http.Cookie
				for _, c := range w.Result().Cookies() {
					if c.Name == "refresh_token" {
						tokenCookie = c
						break
					}
				}
				require.NotNil(t, tokenCookie)
				assert.Equal(t, tt.expectedCookie, tokenCookie.Value)
			}
		})
	}
}

func TestRouter_HandleDeleteUser(t *testing.T) {
	t.Parallel()

	const userID = "11111111-1111-1111-1111-111111111111"

	tests := []struct {
		name         string
		userIDInCtx  string
		body         any
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
		expectCookie bool
	}{
		{
			name:        "Success: 비밀번호 일치 시 204 + 쿠키 만료",
			userIDInCtx: userID,
			body:        DeleteUserRequest{Password: "SecurePass123!"},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().DeleteUser(mock.Anything, &userpb.DeleteUserRequest{
					UserId:   userID,
					Password: "SecurePass123!",
				}).Return(&userpb.DeleteUserResponse{}, nil)
			},
			expectedCode: http.StatusNoContent,
			expectCookie: true,
		},
		{
			name:         "Failure: JWT 미들웨어 미통과 (Unauthorized)",
			userIDInCtx:  "",
			body:         DeleteUserRequest{Password: "SecurePass123!"},
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Failure: 잘못된 JSON 본문",
			userIDInCtx:  userID,
			body:         "not-json",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Failure: 비밀번호 누락",
			userIDInCtx:  userID,
			body:         DeleteUserRequest{Password: ""},
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:        "Failure: 비밀번호 불일치 (gRPC Unauthenticated → 401)",
			userIDInCtx: userID,
			body:        DeleteUserRequest{Password: "wrong"},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().DeleteUser(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Unauthenticated, "invalid password"))
			},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:        "Failure: 사용자 없음 (gRPC NotFound → 404)",
			userIDInCtx: userID,
			body:        DeleteUserRequest{Password: "SecurePass123!"},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().DeleteUser(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.NotFound, "user not found"))
			},
			expectedCode: http.StatusNotFound,
		},
		{
			name:        "Failure: 내부 오류 (gRPC Internal → 500)",
			userIDInCtx: userID,
			body:        DeleteUserRequest{Password: "SecurePass123!"},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().DeleteUser(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Internal, "boom"))
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
				},
			}

			var reqBody []byte
			if s, ok := tt.body.(string); ok {
				reqBody = []byte(s)
			} else {
				var err error
				reqBody, err = json.Marshal(tt.body)
				require.NoError(t, err)
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodDelete, "/me", bytes.NewBuffer(reqBody))
			req.Header.Set("Content-Type", "application/json")
			if tt.userIDInCtx != "" {
				ctx := context.WithValue(req.Context(), middleware.UserIDKey, tt.userIDInCtx)
				req = req.WithContext(ctx)
			}

			r.handleDeleteUser(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)

			if tt.expectCookie {
				var cookie *http.Cookie
				for _, c := range w.Result().Cookies() {
					if c.Name == "refresh_token" {
						cookie = c
						break
					}
				}
				require.NotNil(t, cookie)
				assert.Equal(t, -1, cookie.MaxAge)
			}
		})
	}
}

