package apigateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/shared/auth"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/middleware"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRouter_HandleListJoinedRooms(t *testing.T) {
	t.Parallel()
	jwtSecret := "test-secret"
	validToken, _ := auth.GenerateJWT("user-1", "testuser", jwtSecret, time.Hour)

	tests := []struct {
		name         string
		token        string
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
		expectedBody any
	}{
		{
			name:  "Success: 참여 중인 채팅방 목록 조회",
			token: validToken,
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().ListJoinedRooms(mock.Anything, &userpb.ListJoinedRoomsRequest{
					UserId: "user-1",
				}).Return(&userpb.ListJoinedRoomsResponse{
					Rooms: []*userpb.UserRoom{
						{
							Room:     &userpb.Room{Id: "room-1", Name: "My Room", ManagerId: "user-1", Capacity: 100, MemberCount: 1},
							JoinedAt: timestamppb.Now(),
						},
					},
				}, nil)
			},
			expectedCode: http.StatusOK,
			expectedBody: map[string]any{
				"rooms": []any{
					map[string]any{
						"id":         "room-1",
						"name":       "My Room",
						"manager_id":    "user-1",
						"capacity":      float64(100),
						"member_count":  float64(1),
					},
				},
			},
		},
		{
			name:         "Failure: Authorization 헤더 누락 (Unauthorized)",
			token:        "",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
			expectedBody: map[string]any{"detail": "missing token"},
		},
		{
			name:         "Failure: 잘못된 형식의 JWT 토큰 (Unauthorized)",
			token:        "invalid.token.string",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
			expectedBody: map[string]any{"detail": "invalid token"},
		},
		{
			name:  "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			token: validToken,
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().ListJoinedRooms(mock.Anything, mock.Anything).
					Return(nil, errors.New("service failure"))
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
				jwtSecret:  jwtSecret,
				config:     &Config{AppConfig: config.AppConfig{Env: "test"}},
			}

			mux := http.NewServeMux()
			authMw := middleware.BearerAuthMiddleware(jwtSecret)
			mux.Handle("GET /me/rooms", authMw(http.HandlerFunc(r.handleListJoinedRooms)))

			req := httptest.NewRequest("GET", "/me/rooms", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
			if tt.expectedBody != nil {
				var gotBody map[string]any
				err := json.Unmarshal(w.Body.Bytes(), &gotBody)
				require.NoError(t, err)

				if expectedMap, ok := tt.expectedBody.(map[string]any); ok {
					if _, ok := expectedMap["rooms"]; ok {
						assert.NotNil(t, gotBody["rooms"])
					}
					if detail, ok := expectedMap["detail"]; ok {
						assert.Equal(t, detail, gotBody["detail"])
					}
				}
			}
		})
	}
}

func TestRouter_HandleCreateRoom(t *testing.T) {
	t.Parallel()
	jwtSecret := "test-secret"
	validToken, _ := auth.GenerateJWT("user-1", "testuser", jwtSecret, time.Hour)

	tests := []struct {
		name         string
		token        string
		body         any
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:  "Success: 유효한 요청으로 채팅방 생성 성공",
			token: validToken,
			body: map[string]any{
				"name":     "New Room",
				"capacity": 50,
			},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().CreateRoom(mock.Anything, &userpb.CreateRoomRequest{
					Name:      "New Room",
					ManagerId: "user-1",
					Capacity:  50,
				}).Return(&userpb.CreateRoomResponse{
					RoomId: "new-room-id",
				}, nil)
			},
			expectedCode: http.StatusCreated,
		},
		{
			name:         "Failure: 잘못된 형식의 요청 본문 (BadRequest)",
			token:        validToken,
			body:         "invalid-body",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockUserClient := mocks.NewMockUserServiceClient(t)
			tt.mockBehavior(mockUserClient)

			r := &Router{
				userClient: mockUserClient,
				jwtSecret:  jwtSecret,
				config:     &Config{AppConfig: config.AppConfig{Env: "test"}},
			}

			mux := http.NewServeMux()
			authMw := middleware.BearerAuthMiddleware(jwtSecret)
			mux.Handle("POST /rooms", authMw(http.HandlerFunc(r.handleCreateRoom)))

			var reqBody []byte
			if s, ok := tt.body.(string); ok {
				reqBody = []byte(s)
			} else {
				var err error
				reqBody, err = json.Marshal(tt.body)
				require.NoError(t, err)
			}

			req := httptest.NewRequest("POST", "/rooms", bytes.NewBuffer(reqBody))
			req.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleDeleteRoom(t *testing.T) {
	t.Parallel()
	jwtSecret := "test-secret"
	validToken, _ := auth.GenerateJWT("user-1", "testuser", jwtSecret, time.Hour)

	tests := []struct {
		name         string
		token        string
		roomID       string
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:   "Success: 채팅방 정상 삭제",
			token:  validToken,
			roomID: "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().DeleteRoom(mock.Anything, &userpb.DeleteRoomRequest{
					RoomId:      "room-123",
					RequesterId: "user-1",
				}).Return(&userpb.DeleteRoomResponse{}, nil)
			},
			expectedCode: http.StatusNoContent,
		},
		{
			name:         "Failure: 토큰 없이 삭제 시도 (Unauthorized)",
			token:        "",
			roomID:       "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:   "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			token:  validToken,
			roomID: "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().DeleteRoom(mock.Anything, mock.Anything).
					Return(nil, errors.New("service failure"))
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
				jwtSecret:  jwtSecret,
				httpClient: &http.Client{Timeout: 2 * time.Second},
				config: &Config{
					AppConfig: config.AppConfig{Env: "test"},
					Registry: ServiceRegistry{
						WSGateway: config.HostConfig{Host: "mock-ws-gateway"},
					},
					Port: config.PortConfig{
						WSGateway: "8088",
					},
				},
			}

			mux := http.NewServeMux()
			authMw := middleware.BearerAuthMiddleware(jwtSecret)
			mux.Handle("DELETE /rooms/{id}", authMw(http.HandlerFunc(r.handleDeleteRoom)))

			req := httptest.NewRequest("DELETE", "/rooms/"+tt.roomID, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleJoinRoom(t *testing.T) {
	t.Parallel()
	jwtSecret := "test-secret"
	validToken, _ := auth.GenerateJWT("user-1", "testuser", jwtSecret, time.Hour)

	tests := []struct {
		name         string
		token        string
		roomID       string
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:   "Success: 채팅방 참여 성공",
			token:  validToken,
			roomID: "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().JoinRoom(mock.Anything, &userpb.JoinRoomRequest{
					RoomId: "room-123",
					UserId: "user-1",
				}).Return(&userpb.JoinRoomResponse{}, nil)
			},
			expectedCode: http.StatusNoContent,
		},
		{
			name:         "Failure: 토큰 없이 참여 시도 (Unauthorized)",
			token:        "",
			roomID:       "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:   "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			token:  validToken,
			roomID: "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().JoinRoom(mock.Anything, mock.Anything).
					Return(nil, errors.New("service failure"))
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
				jwtSecret:  jwtSecret,
				httpClient: &http.Client{Timeout: 1 * time.Second},
				config: &Config{
					AppConfig: config.AppConfig{Env: "test"},
					Registry:  ServiceRegistry{WSGateway: config.HostConfig{Host: "localhost"}},
					Port:      config.PortConfig{WSGateway: "8080"},
				},
			}

			mux := http.NewServeMux()
			authMw := middleware.BearerAuthMiddleware(jwtSecret)
			mux.Handle("PUT /rooms/{id}/members/me", authMw(http.HandlerFunc(r.handleJoinRoom)))

			req := httptest.NewRequest("PUT", "/rooms/"+tt.roomID+"/members/me", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleLeaveRoom(t *testing.T) {
	t.Parallel()
	jwtSecret := "test-secret"
	validToken, _ := auth.GenerateJWT("user-1", "testuser", jwtSecret, time.Hour)

	tests := []struct {
		name         string
		token        string
		roomID       string
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:   "Success: 채팅방 나가기 성공",
			token:  validToken,
			roomID: "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().LeaveRoom(mock.Anything, &userpb.LeaveRoomRequest{
					RoomId: "room-123",
					UserId: "user-1",
				}).Return(&userpb.LeaveRoomResponse{}, nil)
			},
			expectedCode: http.StatusNoContent,
		},
		{
			name:         "Failure: 토큰 없이 나가기 시도 (Unauthorized)",
			token:        "",
			roomID:       "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:   "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			token:  validToken,
			roomID: "room-123",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().LeaveRoom(mock.Anything, mock.Anything).
					Return(nil, errors.New("service failure"))
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
				jwtSecret:  jwtSecret,
				httpClient: &http.Client{Timeout: 1 * time.Second},
				config: &Config{
					AppConfig: config.AppConfig{Env: "test"},
					Registry:  ServiceRegistry{WSGateway: config.HostConfig{Host: "localhost"}},
					Port:      config.PortConfig{WSGateway: "8080"},
				},
			}

			mux := http.NewServeMux()
			authMw := middleware.BearerAuthMiddleware(jwtSecret)
			mux.Handle("DELETE /rooms/{id}/members/me", authMw(http.HandlerFunc(r.handleLeaveRoom)))

			req := httptest.NewRequest("DELETE", "/rooms/"+tt.roomID+"/members/me", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleSearchRooms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		query        string
		limit        string
		offset       string
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:   "Success: 쿼리 파라미터를 이용한 채팅방 검색 성공",
			query:  "test",
			limit:  "10",
			offset: "0",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().SearchRooms(mock.Anything, &userpb.SearchRoomsRequest{
					Query:  "test",
					Limit:  10,
					Offset: 0,
				}).Return(&userpb.SearchRoomsResponse{
					Rooms: []*userpb.Room{
						{Id: "room-s1", Name: "test room"},
					},
					TotalCount: 1,
				}, nil)
			},
			expectedCode: http.StatusOK,
		},
		{
			name:         "Failure: 숫자가 아닌 limit 파라미터 (BadRequest)",
			query:        "test",
			limit:        "abc",
			offset:       "0",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Failure: 음수 limit 파라미터 (BadRequest)",
			query:        "test",
			limit:        "-1",
			offset:       "0",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Failure: 최대값 초과 limit 파라미터 (BadRequest)",
			query:        "test",
			limit:        "999",
			offset:       "0",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Failure: 숫자가 아닌 offset 파라미터 (BadRequest)",
			query:        "test",
			limit:        "10",
			offset:       "xyz",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:   "Success: 쿼리 파라미터 없이 전체 목록 조회",
			query:  "",
			limit:  "",
			offset: "",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().SearchRooms(mock.Anything, &userpb.SearchRoomsRequest{
					Query:  "",
					Limit:  20,
					Offset: 0,
				}).Return(&userpb.SearchRoomsResponse{
					Rooms:      []*userpb.Room{},
					TotalCount: 0,
				}, nil)
			},
			expectedCode: http.StatusOK,
		},
		{
			name:   "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			query:  "test",
			limit:  "10",
			offset: "0",
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().SearchRooms(mock.Anything, mock.Anything).
					Return(nil, errors.New("service failure"))
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
						Search: config.SearchConfig{
							DefaultLimit: 20,
							MaxLimit:     100,
						},
					},
				},
			}

			handler := http.HandlerFunc(r.handleSearchRooms)
			req := httptest.NewRequest("GET", "/rooms?q="+tt.query+"&limit="+tt.limit+"&offset="+tt.offset, nil)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)
			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleUpdateRoom(t *testing.T) {
	t.Parallel()
	jwtSecret := "test-secret"
	validToken, _ := auth.GenerateJWT("user-1", "testuser", jwtSecret, time.Hour)

	tests := []struct {
		name         string
		token        string
		roomID       string
		body         any
		mockBehavior func(m *mocks.MockUserServiceClient)
		expectedCode int
	}{
		{
			name:   "Success: 채팅방 정보 정상 수정",
			token:  validToken,
			roomID: "room-123",
			body:   map[string]any{"name": "new name", "capacity": 100},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().UpdateRoom(mock.Anything, &userpb.UpdateRoomRequest{
					Id:          "room-123",
					Name:        "new name",
					Capacity:    100,
					RequesterId: "user-1",
				}).Return(&userpb.UpdateRoomResponse{}, nil)
			},
			expectedCode: http.StatusNoContent,
		},
		{
			name:         "Failure: 토큰 없이 수정 시도 (Unauthorized)",
			token:        "",
			roomID:       "room-123",
			body:         map[string]any{"name": "new name"},
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Failure: 잘못된 형식의 요청 본문 (BadRequest)",
			token:        validToken,
			roomID:       "room-123",
			body:         "invalid-body",
			mockBehavior: func(m *mocks.MockUserServiceClient) {},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:   "Failure: 유저 서비스 내부 에러 발생 (Internal)",
			token:  validToken,
			roomID: "room-123",
			body:   map[string]any{"name": "new name", "capacity": 50},
			mockBehavior: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().UpdateRoom(mock.Anything, mock.Anything).
					Return(nil, errors.New("service failure"))
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
				jwtSecret:  jwtSecret,
				config:     &Config{},
			}

			mux := http.NewServeMux()
			authMw := middleware.BearerAuthMiddleware(jwtSecret)
			mux.Handle("PATCH /rooms/{id}", authMw(http.HandlerFunc(r.handleUpdateRoom)))

			var bodyBytes []byte
			if s, ok := tt.body.(string); ok {
				bodyBytes = []byte(s)
			} else {
				bodyBytes, _ = json.Marshal(tt.body)
			}
			req := httptest.NewRequest("PATCH", "/rooms/"+tt.roomID, bytes.NewBuffer(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()

			mux.ServeHTTP(w, req)
			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleMessages(t *testing.T) {
	t.Parallel()
	jwtSecret := "test-secret"
	validToken, _ := auth.GenerateJWT("user-1", "testuser", jwtSecret, time.Hour)

	tests := []struct {
		name         string
		roomID       string
		lastSeq      string
		limit        string
		mockUser     func(m *mocks.MockUserServiceClient)
		mockChat     func(m *mocks.MockChatServiceClient)
		expectedCode int
	}{
		{
			name:    "Success: last_seq가 없는 경우 ListMessages 호출",
			roomID:  "room-123",
			lastSeq: "",
			limit:   "10",
			mockUser: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().GetMemberJoinedAt(mock.Anything, mock.Anything).Return(&userpb.GetMemberJoinedAtResponse{JoinedAt: timestamppb.Now()}, nil)
			},
			mockChat: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().ListMessages(mock.Anything, mock.Anything).Return(&chatpb.ListMessagesResponse{}, nil)
			},
			expectedCode: http.StatusOK,
		},
		{
			name:    "Success: last_seq가 있는 경우 SyncMessages 호출",
			roomID:  "room-123",
			lastSeq: "5",
			limit:   "10",
			mockUser: func(m *mocks.MockUserServiceClient) {
				m.EXPECT().GetMemberJoinedAt(mock.Anything, mock.Anything).Return(&userpb.GetMemberJoinedAtResponse{JoinedAt: timestamppb.Now()}, nil)
			},
			mockChat: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().SyncMessages(mock.Anything, mock.Anything).Return(&chatpb.SyncMessagesResponse{}, nil)
			},
			expectedCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockUserClient := mocks.NewMockUserServiceClient(t)
			mockChatClient := mocks.NewMockChatServiceClient(t)
			tt.mockUser(mockUserClient)
			tt.mockChat(mockChatClient)

			r := &Router{
				userClient: mockUserClient,
				chatClient: mockChatClient,
				jwtSecret:  jwtSecret,
				config:     &Config{},
			}

			mux := http.NewServeMux()
			authMw := middleware.BearerAuthMiddleware(jwtSecret)
			mux.Handle("GET /rooms/{id}/messages", authMw(http.HandlerFunc(r.handleListMessages)))

			path := "/rooms/" + tt.roomID + "/messages?limit=" + tt.limit
			if tt.lastSeq != "" {
				path += "&last_seq=" + tt.lastSeq
			}
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Authorization", "Bearer "+validToken)
			w := httptest.NewRecorder()

			mux.ServeHTTP(w, req)
			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}
