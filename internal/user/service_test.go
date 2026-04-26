package user

import (
	"context"
	"testing"
	"time"

	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/user/db"
	dbmocks "go-chat-msa/internal/user/db/mocks"
	"go-chat-msa/internal/user/hasher"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestService_CreateUser(t *testing.T) {
	t.Parallel()
	username := "testuser"
	password := "securePass123"
	uid := uuid.New()
	fixedID := pgtype.UUID{Bytes: uid, Valid: true}
	fixedTime := pgtype.Timestamptz{Time: time.Now().Truncate(time.Second), Valid: true}

	req := &pb.CreateUserRequest{
		Username: username,
		Password: password,
	}

	type fields struct {
		mockBehavior func(m *dbmocks.MockQuerier)
	}

	tests := []struct {
		name    string
		fields  fields
		req     *pb.CreateUserRequest
		want    *pb.CreateUserResponse
		wantErr bool
		errCode codes.Code
	}{
		{
			name: "Success: 유효한 회원가입 요청",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						CreateUser(mock.Anything, mock.Anything).
						Return(db.CreateUserRow{
							ID:        fixedID,
							Username:  username,
							CreatedAt: fixedTime,
						}, nil)
				},
			},
			want: &pb.CreateUserResponse{
				UserId: fixedID.String(),
			},
			wantErr: false,
		},
		{
			name: "Failure: 유저네임이 너무 짧은 경우 (InvalidArgument)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {},
			},
			req:     &pb.CreateUserRequest{Username: "a", Password: password},
			wantErr: true,
			errCode: codes.InvalidArgument,
		},
		{
			name: "Failure: 유저네임이 너무 긴 경우 (InvalidArgument)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {},
			},
			req:     &pb.CreateUserRequest{Username: "verylongusername", Password: password},
			wantErr: true,
			errCode: codes.InvalidArgument,
		},
		{
			name: "Failure: 유저네임에 특수문자가 포함된 경우 (InvalidArgument)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {},
			},
			req:     &pb.CreateUserRequest{Username: "user!", Password: password},
			wantErr: true,
			errCode: codes.InvalidArgument,
		},
		{
			name: "Failure: 유저네임에 공백이 포함된 경우 (InvalidArgument)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {},
			},
			req:     &pb.CreateUserRequest{Username: "user name", Password: password},
			wantErr: true,
			errCode: codes.InvalidArgument,
		},
		{
			name: "Success: 유저네임에 한글 허용",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						CreateUser(mock.Anything, mock.Anything).
						Return(db.CreateUserRow{
							ID:        fixedID,
							Username:  "홍길동",
							CreatedAt: fixedTime,
						}, nil)
				},
			},
			req:     &pb.CreateUserRequest{Username: "홍길동", Password: password},
			want:    &pb.CreateUserResponse{UserId: fixedID.String()},
			wantErr: false,
		},
		{
			name: "Failure: 비밀번호가 너무 짧은 경우 (InvalidArgument)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {},
			},
			req:     &pb.CreateUserRequest{Username: username, Password: "short"},
			wantErr: true,
			errCode: codes.InvalidArgument,
		},
		{
			name: "Failure: 비밀번호 최소 길이를 만족하지 못하는 경우 (InvalidArgument)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {},
			},
			req:     &pb.CreateUserRequest{Username: username, Password: "Pass1234!"},
			wantErr: true,
			errCode: codes.InvalidArgument,
		},
		{
			name: "Failure: 비밀번호 복잡도 미달 (2종류만 포함) (InvalidArgument)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {},
			},
			req:     &pb.CreateUserRequest{Username: username, Password: "password12345"},
			wantErr: true,
			errCode: codes.InvalidArgument,
		},
		{
			name: "Success: 비밀번호 복잡도 만족 (3종류 포함)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						CreateUser(mock.Anything, mock.Anything).
						Return(db.CreateUserRow{
							ID:        fixedID,
							Username:  username,
							CreatedAt: fixedTime,
						}, nil)
				},
			},
			req:     &pb.CreateUserRequest{Username: username, Password: "SecurePass1"},
			want:    &pb.CreateUserResponse{UserId: fixedID.String()},
			wantErr: false,
		},
		{
			name: "Failure: 이미 존재하는 유저네임 (AlreadyExists)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						CreateUser(mock.Anything, mock.Anything).
						Return(db.CreateUserRow{}, &pgconn.PgError{Code: "23505"})
				},
			},
			want:    nil,
			wantErr: true,
			errCode: codes.AlreadyExists,
		},
		{
			name: "Failure: 데이터베이스 내부 에러 (Internal)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						CreateUser(mock.Anything, mock.Anything).
						Return(db.CreateUserRow{}, assert.AnError)
				},
			},
			want:    nil,
			wantErr: true,
			errCode: codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.fields.mockBehavior(mockQueries)

			cfg := config.UserConfig{
				Validation: config.ValidationConfig{
					MinUsernameLength: 2,
					MaxUsernameLength: 10,
					MinPasswordLength: 10,
					MaxPasswordLength: 30,
				},
				Room: config.RoomConfig{
					MaxCapacity:   500,
					MaxNameLength: 50,
				},
				Search: config.SearchConfig{
					DefaultLimit: 20,
					MaxLimit:     100,
				},
			}

			h := hasher.NewPool(hasher.PoolConfig{Workers: 1, Buffer: 10})
			defer h.Close()
			s := NewService(mockQueries, cfg, "test_secret", h)

			targetReq := req
			if tt.req != nil {
				targetReq = tt.req
			}

			got, err := s.CreateUser(t.Context(), targetReq)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.errCode, status.Code(err))
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, tt.want.UserId, got.UserId)
			}
		})
	}
}

func TestService_VerifyUser(t *testing.T) {
	t.Parallel()
	username := "loginUser"
	password := "loginPass123"

	hashedBytes, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	validHash := string(hashedBytes)

	uid := uuid.New()
	fixedID := pgtype.UUID{Bytes: uid, Valid: true}
	fixedTime := pgtype.Timestamptz{Time: time.Now().Truncate(time.Second), Valid: true}

	type fields struct {
		mockBehavior func(m *dbmocks.MockQuerier)
	}
	type args struct {
		req *pb.VerifyUserRequest
	}

	tests := []struct {
		name    string
		fields  fields
		args    args
		want    *pb.VerifyUserResponse
		wantErr bool
		errCode codes.Code
	}{
		{
			name: "Success: 로그인 성공",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						GetUserByUsername(mock.Anything, username).
						Return(db.User{
							ID:           fixedID,
							Username:     username,
							PasswordHash: validHash,
							CreatedAt:    fixedTime,
						}, nil)
					m.EXPECT().
						CreateRefreshToken(mock.Anything, mock.Anything).
						Return(nil)
				},
			},
			args: args{
				req: &pb.VerifyUserRequest{Username: username, Password: password},
			},
			want: &pb.VerifyUserResponse{
				UserId:      fixedID.String(),
				AccessToken: "mock_token",
			},
			wantErr: false,
		},
		{
			name: "Failure: 비밀번호 불일치 (Unauthenticated)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						GetUserByUsername(mock.Anything, username).
						Return(db.User{
							ID:           fixedID,
							Username:     username,
							PasswordHash: validHash,
							CreatedAt:    fixedTime,
						}, nil)
				},
			},
			args: args{
				req: &pb.VerifyUserRequest{Username: username, Password: "WrongPassword"},
			},
			want:    nil,
			wantErr: true,
			errCode: codes.Unauthenticated,
		},
		{
			name: "Failure: 존재하지 않는 유저 (Unauthenticated)",
			fields: fields{
				mockBehavior: func(m *dbmocks.MockQuerier) {
					m.EXPECT().
						GetUserByUsername(mock.Anything, "unknown").
						Return(db.User{}, pgx.ErrNoRows)
				},
			},
			args: args{
				req: &pb.VerifyUserRequest{Username: "unknown", Password: password},
			},
			want:    nil,
			wantErr: true,
			errCode: codes.Unauthenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.fields.mockBehavior(mockQueries)

			s := createTestService(mockQueries)

			got, err := s.VerifyUser(t.Context(), tt.args.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.errCode, status.Code(err))
				assert.Contains(t, err.Error(), "invalid username or password")
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, tt.want.UserId, got.UserId)
				assert.NotEmpty(t, got.AccessToken)
			}
		})
	}
}

func TestService_CreateRoom(t *testing.T) {
	t.Parallel()
	roomName := "My Room"
	uid := uuid.New()
	roomUUID := uuid.New()
	fixedRoomID := pgtype.UUID{Bytes: roomUUID, Valid: true}
	fixedTime := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	req := &pb.CreateRoomRequest{
		Name:      roomName,
		ManagerId: uid.String(),
		Capacity:  500,
	}

	tests := []struct {
		name         string
		req          *pb.CreateRoomRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		want         *pb.CreateRoomResponse
		wantErr      bool
		errCode      codes.Code
	}{
		{
			name: "Success: 채팅방 생성 및 멤버 추가 (방장 포함)",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().CreateRoom(mock.Anything, mock.Anything).
					Return(db.CreateRoomRow{
						ID:        fixedRoomID,
						Name:      roomName,
						ManagerID: pgtype.UUID{Bytes: uid, Valid: true},
						Capacity:  500,
						CreatedAt: fixedTime,
					}, nil)

				m.EXPECT().CreateRoomMember(mock.Anything, mock.Anything).
					Return(nil).Times(1)
			},
			want: &pb.CreateRoomResponse{
				RoomId: fixedRoomID.String(),
			},
			wantErr: false,
		},
		{
			name: "Failure: 채팅방 정원 미지정 (InvalidArgument)",
			req: &pb.CreateRoomRequest{
				Name:      roomName,
				ManagerId: uid.String(),
				Capacity:  0,
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 채팅방 정원 상한 초과 (InvalidArgument)",
			req: &pb.CreateRoomRequest{
				Name:      roomName,
				ManagerId: uid.String(),
				Capacity:  1001,
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 채팅방 이름 최대 길이 초과 (InvalidArgument)",
			req: &pb.CreateRoomRequest{
				Name:      string(make([]rune, 51)),
				ManagerId: uid.String(),
				Capacity:  10,
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			targetReq := req
			if tt.req != nil {
				targetReq = tt.req
			}
			got, err := s.CreateRoom(t.Context(), targetReq)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.errCode, status.Code(err))
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, tt.want.RoomId, got.RoomId)
			}
		})
	}
}

func TestService_ListJoinedRooms(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	req := &pb.ListJoinedRoomsRequest{UserId: uid.String()}

	tests := []struct {
		name         string
		mockBehavior func(m *dbmocks.MockQuerier)
		wantLen      int
		wantErr      bool
	}{
		{
			name: "Success: 참여 중인 채팅방 목록 조회",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().ListJoinedRooms(mock.Anything, pgtype.UUID{Bytes: uid, Valid: true}).
					Return([]db.ListJoinedRoomsRow{
						{
							ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
							Name:      "Room 1",
							ManagerID: pgtype.UUID{Bytes: uid, Valid: true},
							Capacity:  1000,
						},
					}, nil)
			},
			wantLen: 1,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.ListJoinedRooms(t.Context(), req)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Len(t, got.Rooms, tt.wantLen)
			}
		})
	}
}

func TestService_JoinRoom(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	roomUUID := uuid.New()

	req := &pb.JoinRoomRequest{
		UserId: uid.String(),
		RoomId: roomUUID.String(),
	}

	tests := []struct {
		name         string
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		errCode      codes.Code
	}{
		{
			name: "Success: 정상 참여",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, mock.Anything).
					Return(db.GetRoomForUpdateRow{Capacity: 10}, nil)
				m.EXPECT().GetRoomMemberCount(mock.Anything, mock.Anything).
					Return(int64(0), nil)
				m.EXPECT().CreateRoomMember(mock.Anything, mock.Anything).
					Return(nil)
			},
			wantErr: false,
		},
		{
			name: "Failure: 방 없음 (NotFound)",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, mock.Anything).
					Return(db.GetRoomForUpdateRow{}, pgx.ErrNoRows)
			},
			wantErr: true,
			errCode: codes.NotFound,
		},
		{
			name: "Failure: 정원 초과 (FailedPrecondition)",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, mock.Anything).
					Return(db.GetRoomForUpdateRow{Capacity: 10}, nil)
				m.EXPECT().GetRoomMemberCount(mock.Anything, mock.Anything).
					Return(int64(10), nil)
			},
			wantErr: true,
			errCode: codes.FailedPrecondition,
		},
		{
			name: "Failure: DB 에러 (Internal)",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, mock.Anything).
					Return(db.GetRoomForUpdateRow{}, assert.AnError)
			},
			wantErr: true,
			errCode: codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.JoinRoom(t.Context(), req)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errCode != codes.OK {
					assert.Equal(t, tt.errCode, status.Code(err))
				}
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
			}
		})
	}
}

func TestService_SearchRooms(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	fixedID := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	tests := []struct {
		name         string
		req          *pb.SearchRoomsRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantCount    int
		totalCount   int32
		wantErr      bool
	}{
		{
			name: "Success: 채팅방 검색 성공",
			req:  &pb.SearchRoomsRequest{Query: "test", Limit: 10, Offset: 0},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().SearchRooms(mock.Anything, db.SearchRoomsParams{
					Column1: pgtype.Text{String: "test", Valid: true},
					Limit:   10,
					Offset:  0,
				}).Return([]db.SearchRoomsRow{
					{
						ID:         fixedID,
						Name:       "Test Room",
						ManagerID:  pgtype.UUID{Bytes: uid, Valid: true},
						Capacity:   1000,
						TotalCount: 1,
					},
				}, nil)
			},
			wantCount:  1,
			totalCount: 1,
			wantErr:    false,
		},
		{
			name: "Failure: 검색 제한 상한 초과 (InvalidArgument)",
			req:  &pb.SearchRoomsRequest{Query: "test", Limit: 101, Offset: 0},
			mockBehavior: func(m *dbmocks.MockQuerier) {

			},
			wantCount:  0,
			totalCount: 0,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.SearchRooms(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Len(t, got.Rooms, tt.wantCount)
				assert.Equal(t, tt.totalCount, got.TotalCount)
			}
		})
	}
}

func TestService_GetMemberJoinedAt(t *testing.T) {
	t.Parallel()
	roomID := uuid.New()
	userID := uuid.New()
	fixedTime := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	tests := []struct {
		name         string
		req          *pb.GetMemberJoinedAtRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		wantCode     codes.Code
	}{
		{
			name: "Success: 멤버 합류 시점 조회 성공",
			req:  &pb.GetMemberJoinedAtRequest{RoomId: roomID.String(), UserId: userID.String()},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetMemberJoinedAt(mock.Anything, db.GetMemberJoinedAtParams{
					RoomID: pgtype.UUID{Bytes: roomID, Valid: true},
					UserID: pgtype.UUID{Bytes: userID, Valid: true},
				}).Return(fixedTime, nil)
			},
			wantErr: false,
		},
		{
			name: "Failure: 채팅방 멤버가 아닌 경우 (NotFound)",
			req:  &pb.GetMemberJoinedAtRequest{RoomId: roomID.String(), UserId: userID.String()},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetMemberJoinedAt(mock.Anything, mock.Anything).
					Return(pgtype.Timestamptz{}, pgx.ErrNoRows)
			},
			wantErr:  true,
			wantCode: codes.NotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.GetMemberJoinedAt(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				assert.Equal(t, tt.wantCode, status.Code(err))
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.NotNil(t, got.JoinedAt)
			}
		})
	}
}

func TestService_VerifyRoomMember(t *testing.T) {
	t.Parallel()
	roomID := uuid.New()
	userID := uuid.New()

	tests := []struct {
		name         string
		req          *pb.VerifyRoomMemberRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		wantCode     codes.Code
	}{
		{
			name: "Success: 채팅방 멤버 여부 확인 성공",
			req:  &pb.VerifyRoomMemberRequest{RoomId: roomID.String(), UserId: userID.String()},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().ExistsRoomMember(mock.Anything, db.ExistsRoomMemberParams{
					RoomID: pgtype.UUID{Bytes: roomID, Valid: true},
					UserID: pgtype.UUID{Bytes: userID, Valid: true},
				}).Return(true, nil)
			},
			wantErr: false,
		},
		{
			name: "Failure: 채팅방 멤버가 아닌 경우 (NotFound)",
			req:  &pb.VerifyRoomMemberRequest{RoomId: roomID.String(), UserId: userID.String()},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().ExistsRoomMember(mock.Anything, mock.Anything).
					Return(false, nil)
			},
			wantErr:  true,
			wantCode: codes.NotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.VerifyRoomMember(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				assert.Equal(t, tt.wantCode, status.Code(err))
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
			}
		})
	}
}

func TestService_UpdateRoom(t *testing.T) {
	t.Parallel()
	roomID := uuid.New()
	managerID := uuid.New()
	otherUserID := uuid.New()
	roomPGUUID := pgtype.UUID{Bytes: roomID, Valid: true}
	managerPGUUID := pgtype.UUID{Bytes: managerID, Valid: true}

	tests := []struct {
		name         string
		req          *pb.UpdateRoomRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		errCode      codes.Code
	}{
		{
			name: "Success: 채팅방 정보 정상 수정",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        "New Name",
				Capacity:    500,
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{
						ID:        roomPGUUID,
						ManagerID: managerPGUUID,
						Capacity:  1000,
					}, nil)
				m.EXPECT().GetRoomMemberCount(mock.Anything, roomPGUUID).
					Return(int64(3), nil)
				m.EXPECT().UpdateRoom(mock.Anything, db.UpdateRoomParams{
					ID:        roomPGUUID,
					Name:      "New Name",
					Capacity:  500,
					ManagerID: managerPGUUID,
				}).Return(roomPGUUID, nil)
			},
			wantErr: false,
		},
		{
			name: "Failure: 채팅방 이름 누락 (InvalidArgument)",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        "",
				Capacity:    500,
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 채팅방 이름 최대 길이 초과 (InvalidArgument)",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        string(make([]rune, 51)),
				Capacity:    500,
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 채팅방 정원 미설정 (InvalidArgument)",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        "New Name",
				Capacity:    0,
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 채팅방 정원 상한 초과 (InvalidArgument)",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        "New Name",
				Capacity:    1001,
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 방장이 아닌 사용자가 수정 시도 (PermissionDenied)",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        "New Name",
				Capacity:    500,
				RequesterId: otherUserID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{
						ID:        roomPGUUID,
						ManagerID: managerPGUUID,
						Capacity:  1000,
					}, nil)
			},
			wantErr: true,
			errCode: codes.PermissionDenied,
		},
		{
			name: "Failure: 존재하지 않는 채팅방 (NotFound)",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        "New Name",
				Capacity:    500,
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{}, pgx.ErrNoRows)
			},
			wantErr: true,
			errCode: codes.NotFound,
		},
		{
			name: "Failure: 정원을 현재 인원 수 미만으로 축소 (FailedPrecondition)",
			req: &pb.UpdateRoomRequest{
				Id:          roomID.String(),
				Name:        "New Name",
				Capacity:    3,
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{
						ID:        roomPGUUID,
						ManagerID: managerPGUUID,
						Capacity:  10,
					}, nil)
				m.EXPECT().GetRoomMemberCount(mock.Anything, roomPGUUID).
					Return(int64(5), nil)
			},
			wantErr: true,
			errCode: codes.FailedPrecondition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.UpdateRoom(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.errCode, status.Code(err))
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
			}
		})
	}
}

func TestService_LeaveRoom(t *testing.T) {
	t.Parallel()
	roomID := uuid.New()
	managerID := uuid.New()
	newManagerID := uuid.New()

	tests := []struct {
		name         string
		req          *pb.LeaveRoomRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		errCode      codes.Code
	}{
		{
			name: "Success: 방장이 나갈 때 가장 오래된 멤버에게 방장 권한 위임",
			req: &pb.LeaveRoomRequest{
				RoomId: roomID.String(),
				UserId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, pgtype.UUID{Bytes: roomID, Valid: true}).Return(db.GetRoomForUpdateRow{
					ID:        pgtype.UUID{Bytes: roomID, Valid: true},
					ManagerID: pgtype.UUID{Bytes: managerID, Valid: true},
				}, nil)
				m.EXPECT().DeleteRoomMember(mock.Anything, db.DeleteRoomMemberParams{
					RoomID: pgtype.UUID{Bytes: roomID, Valid: true},
					UserID: pgtype.UUID{Bytes: managerID, Valid: true},
				}).Return(nil)
				m.EXPECT().GetOldestRoomMember(mock.Anything, pgtype.UUID{Bytes: roomID, Valid: true}).Return(pgtype.UUID{Bytes: newManagerID, Valid: true}, nil)
				m.EXPECT().UpdateRoomManager(mock.Anything, db.UpdateRoomManagerParams{
					ID:        pgtype.UUID{Bytes: roomID, Valid: true},
					ManagerID: pgtype.UUID{Bytes: newManagerID, Valid: true},
				}).Return(nil)
			},
			wantErr: false,
		},
		{
			name: "Success: 마지막 멤버(방장)가 나갈 때 방 soft delete",
			req: &pb.LeaveRoomRequest{
				RoomId: roomID.String(),
				UserId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, pgtype.UUID{Bytes: roomID, Valid: true}).Return(db.GetRoomForUpdateRow{
					ID:        pgtype.UUID{Bytes: roomID, Valid: true},
					ManagerID: pgtype.UUID{Bytes: managerID, Valid: true},
				}, nil)
				m.EXPECT().DeleteRoomMember(mock.Anything, db.DeleteRoomMemberParams{
					RoomID: pgtype.UUID{Bytes: roomID, Valid: true},
					UserID: pgtype.UUID{Bytes: managerID, Valid: true},
				}).Return(nil)
				m.EXPECT().GetOldestRoomMember(mock.Anything, pgtype.UUID{Bytes: roomID, Valid: true}).Return(pgtype.UUID{}, pgx.ErrNoRows)
				m.EXPECT().SoftDeleteRoom(mock.Anything, mock.MatchedBy(func(p db.SoftDeleteRoomParams) bool {
					return p.ID == pgtype.UUID{Bytes: roomID, Valid: true} &&
						p.ManagerID == pgtype.UUID{Bytes: managerID, Valid: true} &&
						p.DeletedAt.Valid
				})).Return(pgtype.UUID{Bytes: roomID, Valid: true}, nil)
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.LeaveRoom(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
			}
		})
	}
}

func TestService_DeleteRoom(t *testing.T) {
	t.Parallel()
	roomID := uuid.New()
	managerID := uuid.New()
	otherUserID := uuid.New()
	roomPGUUID := pgtype.UUID{Bytes: roomID, Valid: true}
	managerPGUUID := pgtype.UUID{Bytes: managerID, Valid: true}

	tests := []struct {
		name         string
		req          *pb.DeleteRoomRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		errCode      codes.Code
	}{
		{
			name: "Success: 방장이 채팅방 정상 삭제 (Soft Delete)",
			req: &pb.DeleteRoomRequest{
				RoomId:      roomID.String(),
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{
						ID:        roomPGUUID,
						ManagerID: managerPGUUID,
					}, nil)
				m.EXPECT().SoftDeleteRoom(mock.Anything, mock.Anything).
					Return(roomPGUUID, nil)
			},
			wantErr: false,
		},
		{
			name: "Failure: 잘못된 room_id 형식 (InvalidArgument)",
			req: &pb.DeleteRoomRequest{
				RoomId:      "invalid-uuid",
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 잘못된 requester_id 형식 (InvalidArgument)",
			req: &pb.DeleteRoomRequest{
				RoomId:      roomID.String(),
				RequesterId: "invalid-uuid",
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 방장이 아닌 사용자가 삭제 시도 (PermissionDenied)",
			req: &pb.DeleteRoomRequest{
				RoomId:      roomID.String(),
				RequesterId: otherUserID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{
						ID:        roomPGUUID,
						ManagerID: managerPGUUID,
					}, nil)
			},
			wantErr: true,
			errCode: codes.PermissionDenied,
		},
		{
			name: "Failure: 존재하지 않는 채팅방 (NotFound)",
			req: &pb.DeleteRoomRequest{
				RoomId:      roomID.String(),
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{}, pgx.ErrNoRows)
			},
			wantErr: true,
			errCode: codes.NotFound,
		},
		{
			name: "Failure: 데이터베이스 내부 오류 (Internal)",
			req: &pb.DeleteRoomRequest{
				RoomId:      roomID.String(),
				RequesterId: managerID.String(),
			},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRoomForUpdate(mock.Anything, roomPGUUID).
					Return(db.GetRoomForUpdateRow{
						ID:        roomPGUUID,
						ManagerID: managerPGUUID,
					}, nil)
				m.EXPECT().SoftDeleteRoom(mock.Anything, mock.Anything).
					Return(pgtype.UUID{}, assert.AnError)
			},
			wantErr: true,
			errCode: codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.DeleteRoom(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.errCode, status.Code(err))
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
			}
		})
	}
}

func createTestService(mockQueries db.Querier) *Service {
	cfg := config.UserConfig{
		Room: config.RoomConfig{
			MaxCapacity:   1000,
			MaxNameLength: 50,
		},
		Token: config.TokenConfig{
			AccessTokenExpirationMinutes: 30,
			RefreshTokenExpirationDays:   7,
			TokenPurgeInterval:           time.Hour,
		},
		Search: config.SearchConfig{
			DefaultLimit: 20,
			MaxLimit:     100,
		},
	}
	h := hasher.NewPool(hasher.PoolConfig{Workers: 1, Buffer: 10})
	return NewService(mockQueries, cfg, "test_secret", h).
		WithRunInTx(func(ctx context.Context, fn func(db.Querier) error) error {
			return fn(mockQueries)
		})
}

func TestService_RefreshToken(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	fixedID := pgtype.UUID{Bytes: uid, Valid: true}
	tokenStr := "valid_refresh_token_string"

	tests := []struct {
		name         string
		req          *pb.RefreshTokenRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		errCode      codes.Code
	}{
		{
			name: "Success: 리프레시 토큰으로 액세스 토큰 갱신 성공",
			req:  &pb.RefreshTokenRequest{RefreshToken: tokenStr},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRefreshTokenByHashForUpdate(mock.Anything, mock.Anything).Return(db.RefreshToken{
					ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
					UserID:    fixedID,
					TokenHash: tokenStr,
					ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
					Used:      false,
				}, nil)

				m.EXPECT().MarkRefreshTokenUsed(mock.Anything, mock.Anything).Return(nil)
				m.EXPECT().GetUserByID(mock.Anything, mock.Anything).Return(db.User{
					ID:       fixedID,
					Username: "refresh_user",
				}, nil)
				m.EXPECT().CreateRefreshToken(mock.Anything, mock.Anything).Return(nil)
			},
			wantErr: false,
		},
		{
			name: "Failure: 존재하지 않는 리프레시 토큰 (Unauthenticated)",
			req:  &pb.RefreshTokenRequest{RefreshToken: "invalid_token"},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRefreshTokenByHashForUpdate(mock.Anything, mock.Anything).Return(db.RefreshToken{}, pgx.ErrNoRows)
			},
			wantErr: true,
			errCode: codes.Unauthenticated,
		},
		{
			name: "Failure: 만료된 리프레시 토큰 (Unauthenticated)",
			req:  &pb.RefreshTokenRequest{RefreshToken: "expired_token"},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRefreshTokenByHashForUpdate(mock.Anything, mock.Anything).Return(db.RefreshToken{
					UserID:    fixedID,
					TokenHash: "expired_token",
					ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
					Used:      false,
				}, nil)
			},
			wantErr: true,
			errCode: codes.Unauthenticated,
		},
		{
			name: "Failure: 토큰 소유자가 탈퇴(soft-deleted)되어 조회 불가 (Unauthenticated)",
			req:  &pb.RefreshTokenRequest{RefreshToken: tokenStr},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRefreshTokenByHashForUpdate(mock.Anything, mock.Anything).Return(db.RefreshToken{
					ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
					UserID:    fixedID,
					TokenHash: tokenStr,
					ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
					Used:      false,
				}, nil)
				m.EXPECT().MarkRefreshTokenUsed(mock.Anything, mock.Anything).Return(nil)
				m.EXPECT().GetUserByID(mock.Anything, mock.Anything).Return(db.User{}, pgx.ErrNoRows)
			},
			wantErr: true,
			errCode: codes.Unauthenticated,
		},
		{
			name: "Failure: 이미 사용된 토큰 재사용 시도 차단 (Reuse Detection - Unauthenticated)",
			req:  &pb.RefreshTokenRequest{RefreshToken: "revoked_token"},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetRefreshTokenByHashForUpdate(mock.Anything, mock.Anything).Return(db.RefreshToken{
					UserID:    fixedID,
					TokenHash: "revoked_token",
					ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
					Used:      true,
				}, nil)
				m.EXPECT().DeleteRefreshTokensByUserID(mock.Anything, fixedID).Return(nil)
			},
			wantErr: true,
			errCode: codes.Unauthenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.RefreshToken(context.Background(), tt.req)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errCode, status.Code(err))
				assert.Nil(t, got)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, got)
				assert.NotEmpty(t, got.AccessToken)
			}
		})
	}
}

func TestService_RevokeToken(t *testing.T) {
	t.Parallel()
	req := &pb.RevokeTokenRequest{RefreshToken: "token_to_revoke"}

	tests := []struct {
		name         string
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
	}{
		{
			name: "Success: 리프레시 토큰 무효화(Revoke) 성공",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().DeleteRefreshTokenByHash(mock.Anything, mock.Anything).Return(nil)
			},
			wantErr: false,
		},
		{
			name: "Failure: 데이터베이스 오류로 인한 무효화 실패",
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().DeleteRefreshTokenByHash(mock.Anything, mock.Anything).Return(assert.AnError)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)

			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.RevokeToken(context.Background(), req)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, got)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, got)
			}
		})
	}
}

func TestService_DeleteUser(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	userPGUUID := pgtype.UUID{Bytes: uid, Valid: true}
	password := "validPass123"

	hashedBytes, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	validHash := string(hashedBytes)

	managerRoom := uuid.New()
	memberRoom := uuid.New()
	otherUser := uuid.New()

	tests := []struct {
		name         string
		req          *pb.DeleteUserRequest
		mockBehavior func(m *dbmocks.MockQuerier)
		wantErr      bool
		errCode      codes.Code
		wantRoomLen  int
	}{
		{
			name: "Success: 매니저 방(위임) + 일반 방을 정리하고 토큰 폐기 후 soft delete",
			req:  &pb.DeleteUserRequest{UserId: uid.String(), Password: password},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetUserByID(mock.Anything, userPGUUID).Return(db.User{
					ID:           userPGUUID,
					Username:     "deleter",
					PasswordHash: validHash,
				}, nil)
				m.EXPECT().ListJoinedRoomIDsForUpdate(mock.Anything, userPGUUID).Return([]pgtype.UUID{
					{Bytes: managerRoom, Valid: true},
					{Bytes: memberRoom, Valid: true},
				}, nil)
				// 매니저인 방 — 멤버 남음 → 위임
				m.EXPECT().GetRoomForUpdate(mock.Anything, pgtype.UUID{Bytes: managerRoom, Valid: true}).Return(db.GetRoomForUpdateRow{
					ID:        pgtype.UUID{Bytes: managerRoom, Valid: true},
					ManagerID: userPGUUID,
				}, nil)
				m.EXPECT().DeleteRoomMember(mock.Anything, db.DeleteRoomMemberParams{
					RoomID: pgtype.UUID{Bytes: managerRoom, Valid: true},
					UserID: userPGUUID,
				}).Return(nil)
				m.EXPECT().GetOldestRoomMember(mock.Anything, pgtype.UUID{Bytes: managerRoom, Valid: true}).Return(pgtype.UUID{Bytes: otherUser, Valid: true}, nil)
				m.EXPECT().UpdateRoomManager(mock.Anything, db.UpdateRoomManagerParams{
					ID:        pgtype.UUID{Bytes: managerRoom, Valid: true},
					ManagerID: pgtype.UUID{Bytes: otherUser, Valid: true},
				}).Return(nil)
				// 일반 멤버인 방 — 멤버만 빠짐
				m.EXPECT().GetRoomForUpdate(mock.Anything, pgtype.UUID{Bytes: memberRoom, Valid: true}).Return(db.GetRoomForUpdateRow{
					ID:        pgtype.UUID{Bytes: memberRoom, Valid: true},
					ManagerID: pgtype.UUID{Bytes: otherUser, Valid: true},
				}, nil)
				m.EXPECT().DeleteRoomMember(mock.Anything, db.DeleteRoomMemberParams{
					RoomID: pgtype.UUID{Bytes: memberRoom, Valid: true},
					UserID: userPGUUID,
				}).Return(nil)
				m.EXPECT().DeleteRefreshTokensByUserID(mock.Anything, userPGUUID).Return(nil)
				m.EXPECT().SoftDeleteUser(mock.Anything, mock.MatchedBy(func(p db.SoftDeleteUserParams) bool {
					return p.ID == userPGUUID && p.DeletedAt.Valid
				})).Return(userPGUUID, nil)
			},
			wantErr:     false,
			wantRoomLen: 2,
		},
		{
			name: "Success: 마지막 멤버이자 매니저인 방은 soft-delete",
			req:  &pb.DeleteUserRequest{UserId: uid.String(), Password: password},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetUserByID(mock.Anything, userPGUUID).Return(db.User{
					ID:           userPGUUID,
					Username:     "deleter",
					PasswordHash: validHash,
				}, nil)
				m.EXPECT().ListJoinedRoomIDsForUpdate(mock.Anything, userPGUUID).Return([]pgtype.UUID{
					{Bytes: managerRoom, Valid: true},
				}, nil)
				m.EXPECT().GetRoomForUpdate(mock.Anything, pgtype.UUID{Bytes: managerRoom, Valid: true}).Return(db.GetRoomForUpdateRow{
					ID:        pgtype.UUID{Bytes: managerRoom, Valid: true},
					ManagerID: userPGUUID,
				}, nil)
				m.EXPECT().DeleteRoomMember(mock.Anything, mock.Anything).Return(nil)
				m.EXPECT().GetOldestRoomMember(mock.Anything, pgtype.UUID{Bytes: managerRoom, Valid: true}).Return(pgtype.UUID{}, pgx.ErrNoRows)
				m.EXPECT().SoftDeleteRoom(mock.Anything, mock.MatchedBy(func(p db.SoftDeleteRoomParams) bool {
					return p.ID == pgtype.UUID{Bytes: managerRoom, Valid: true} && p.DeletedAt.Valid
				})).Return(pgtype.UUID{Bytes: managerRoom, Valid: true}, nil)
				m.EXPECT().DeleteRefreshTokensByUserID(mock.Anything, userPGUUID).Return(nil)
				m.EXPECT().SoftDeleteUser(mock.Anything, mock.Anything).Return(userPGUUID, nil)
			},
			wantErr:     false,
			wantRoomLen: 1,
		},
		{
			name: "Success: 가입한 방이 없는 경우",
			req:  &pb.DeleteUserRequest{UserId: uid.String(), Password: password},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetUserByID(mock.Anything, userPGUUID).Return(db.User{
					ID:           userPGUUID,
					Username:     "deleter",
					PasswordHash: validHash,
				}, nil)
				m.EXPECT().ListJoinedRoomIDsForUpdate(mock.Anything, userPGUUID).Return(nil, nil)
				m.EXPECT().DeleteRefreshTokensByUserID(mock.Anything, userPGUUID).Return(nil)
				m.EXPECT().SoftDeleteUser(mock.Anything, mock.Anything).Return(userPGUUID, nil)
			},
			wantErr:     false,
			wantRoomLen: 0,
		},
		{
			name:         "Failure: 잘못된 user_id 형식 (InvalidArgument)",
			req:          &pb.DeleteUserRequest{UserId: "not-a-uuid", Password: password},
			mockBehavior: func(m *dbmocks.MockQuerier) {},
			wantErr:      true,
			errCode:      codes.InvalidArgument,
		},
		{
			name: "Failure: 사용자 없음 (NotFound)",
			req:  &pb.DeleteUserRequest{UserId: uid.String(), Password: password},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetUserByID(mock.Anything, userPGUUID).Return(db.User{}, pgx.ErrNoRows)
			},
			wantErr: true,
			errCode: codes.NotFound,
		},
		{
			name: "Failure: 비밀번호 불일치 (Unauthenticated)",
			req:  &pb.DeleteUserRequest{UserId: uid.String(), Password: "wrongPassword"},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetUserByID(mock.Anything, userPGUUID).Return(db.User{
					ID:           userPGUUID,
					Username:     "deleter",
					PasswordHash: validHash,
				}, nil)
			},
			wantErr: true,
			errCode: codes.Unauthenticated,
		},
		{
			name: "Failure: 이중 탈퇴 (이미 soft-deleted) → NotFound",
			req:  &pb.DeleteUserRequest{UserId: uid.String(), Password: password},
			mockBehavior: func(m *dbmocks.MockQuerier) {
				m.EXPECT().GetUserByID(mock.Anything, userPGUUID).Return(db.User{
					ID:           userPGUUID,
					Username:     "deleter",
					PasswordHash: validHash,
				}, nil)
				m.EXPECT().ListJoinedRoomIDsForUpdate(mock.Anything, userPGUUID).Return(nil, nil)
				m.EXPECT().DeleteRefreshTokensByUserID(mock.Anything, userPGUUID).Return(nil)
				m.EXPECT().SoftDeleteUser(mock.Anything, mock.Anything).Return(pgtype.UUID{}, pgx.ErrNoRows)
			},
			wantErr: true,
			errCode: codes.NotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockQueries := dbmocks.NewMockQuerier(t)
			tt.mockBehavior(mockQueries)

			s := createTestService(mockQueries)
			got, err := s.DeleteUser(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.errCode, status.Code(err))
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Len(t, got.LeftRoomIds, tt.wantRoomLen)
			}
		})
	}
}

func TestService_PurgeExpiredTokens(t *testing.T) {
	t.Parallel()

	t.Run("Success: 백그라운드 리프레시 토큰 정리 루프 작동", func(t *testing.T) {
		t.Parallel()
		mockQueries := dbmocks.NewMockQuerier(t)
		mockQueries.EXPECT().DeleteExpiredRefreshTokens(mock.Anything).Return(nil)

		cfg := config.UserConfig{
			Token: config.TokenConfig{
				TokenPurgeInterval: 5 * time.Millisecond,
			},
		}
		h := hasher.NewPool(hasher.PoolConfig{Workers: 1, Buffer: 10})
		defer h.Close()
		s := NewService(mockQueries, cfg, "test_secret", h)

		ctx, cancel := context.WithCancel(t.Context())
		go s.PurgeExpiredTokens(ctx)

		time.Sleep(20 * time.Millisecond)
		cancel()
		time.Sleep(10 * time.Millisecond)
	})
}
