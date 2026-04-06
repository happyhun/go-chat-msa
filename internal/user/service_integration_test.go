//go:build integration

package user_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/auth"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/database"
	"go-chat-msa/internal/user"
	"go-chat-msa/internal/user/db"
	"go-chat-msa/internal/user/hasher"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type userIntegrationConfig struct {
	UserService config.UserConfig `mapstructure:"USER_SERVICE" validate:"required"`
}

type UserSuite struct {
	suite.Suite
	container *postgres.PostgresContainer
	db        *pgxpool.Pool
	client    *user.Service
}

func (s *UserSuite) SetupSuite() {
	ctx := context.Background()
	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("test_db"),
		postgres.WithUsername("test_user"),
		postgres.WithPassword("test_password"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").
				WithStartupTimeout(10*time.Second)),
	)
	s.Require().NoError(err)
	s.container = pgContainer

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	s.Require().NoError(err)

	s.db, err = database.NewPostgres(connStr)
	s.Require().NoError(err)

	cfgPath, err := filepath.Abs("../../configs")
	s.Require().NoError(err)

	cfg, err := config.Load[userIntegrationConfig](cfgPath, "base", "")
	s.Require().NoError(err)

	s.runMigrations(ctx)

	hp := hasher.NewPool(hasher.DefaultPoolConfig())
	s.client = user.NewService(db.New(s.db), cfg.UserService, "integration_test_secret", hp).
		WithRunInTx(func(ctx context.Context, fn func(db.Querier) error) error {
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)

			if err := fn(db.New(tx)); err != nil {
				return err
			}

			return tx.Commit(ctx)
		})
}

func (s *UserSuite) runMigrations(ctx context.Context) {
	migrationsDir, err := filepath.Abs("../../db/migrations/postgres")
	s.Require().NoError(err)

	connStr, err := s.container.ConnectionString(ctx, "sslmode=disable")
	s.Require().NoError(err)

	migrationURL := "pgx5" + connStr[len("postgres"):]

	m, err := migrate.New("file://"+migrationsDir, migrationURL)
	s.Require().NoError(err)

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		s.Require().NoError(err)
	}

	srcErr, dbErr := m.Close()
	s.Require().NoError(srcErr)
	s.Require().NoError(dbErr)
}

func (s *UserSuite) TearDownSuite() {
	if s.container != nil {
		s.container.Terminate(context.Background())
	}
}

func (s *UserSuite) SetupTest() {
	_, err := s.db.Exec(s.T().Context(), "TRUNCATE TABLE users RESTART IDENTITY CASCADE")
	s.Require().NoError(err)
}

func (s *UserSuite) TestCreateUser_ValidArgs() {
	req := &pb.CreateUserRequest{
		Username: "tester1",
		Password: "SecurePass123!",
	}

	res, err := s.client.CreateUser(s.T().Context(), req)

	s.NoError(err)
	s.NotNil(res)
	s.NotEmpty(res.UserId)
}

func (s *UserSuite) TestCreateUser_InvalidPassword() {
	tests := []struct {
		name     string
		password string
	}{
		{"Failure: 너무 짧은 비밀번호 (InvalidArgument)", "Short1!"},
		{"Failure: 조합 조건 미달 (InvalidArgument)", "password12345"},
		{"Failure: 유효하지 않은 문자 포함 (InvalidArgument)", "SecurePassword 한글"},
		{"Failure: 공백 포함 (InvalidArgument)", "Secure Password 123!"},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			req := &pb.CreateUserRequest{
				Username: "invalpwduser_" + tt.name,
				Password: tt.password,
			}
			res, err := s.client.CreateUser(s.T().Context(), req)
			s.Error(err, "Password should be invalid: %s", tt.password)
			s.Nil(res)
			s.Equal(codes.InvalidArgument, status.Code(err))
		})
	}
}

func (s *UserSuite) TestCreateUser_AlreadyExists() {
	username := "dupeuser"
	req := &pb.CreateUserRequest{
		Username: username,
		Password: "SecurePass123!",
	}
	_, err := s.client.CreateUser(s.T().Context(), req)
	s.Require().NoError(err)

	res, err := s.client.CreateUser(s.T().Context(), req)

	s.Error(err)
	s.Nil(res)

	st, ok := status.FromError(err)
	s.True(ok)
	s.Equal(codes.AlreadyExists, st.Code())
}

func (s *UserSuite) TestVerifyUser_ValidCredentials() {
	username := "loginuser"
	password := "SecurePass123!"

	_, err := s.client.CreateUser(s.T().Context(), &pb.CreateUserRequest{
		Username: username,
		Password: password,
	})
	s.Require().NoError(err)

	res, err := s.client.VerifyUser(s.T().Context(), &pb.VerifyUserRequest{
		Username: username,
		Password: password,
	})

	s.NoError(err)
	s.NotEmpty(res.UserId)
	s.NotEmpty(res.AccessToken)
	s.NotEmpty(res.RefreshToken)

	claims, err := auth.VerifyJWT(res.AccessToken, "integration_test_secret")
	s.NoError(err, "AccessToken verification failed")
	s.Equal(username, claims.Username)
}

func (s *UserSuite) TestRefreshToken_Success() {
	username := "refreshuser"
	password := "SecurePass123!"
	s.createUser(username, password)

	loginRes, err := s.client.VerifyUser(s.T().Context(), &pb.VerifyUserRequest{
		Username: username,
		Password: password,
	})
	s.Require().NoError(err)

	refreshRes, err := s.client.RefreshToken(s.T().Context(), &pb.RefreshTokenRequest{
		RefreshToken: loginRes.RefreshToken,
	})

	s.NoError(err)
	s.NotEmpty(refreshRes.AccessToken)
	s.NotEmpty(refreshRes.RefreshToken)
	s.NotEqual(loginRes.RefreshToken, refreshRes.RefreshToken, "Refresh token should be rotated")

	claims, err := auth.VerifyJWT(refreshRes.AccessToken, "integration_test_secret")
	s.NoError(err)
	s.Equal(username, claims.Username)
}

func (s *UserSuite) TestRefreshToken_ReuseDetection() {
	username := "reuseuser"
	s.createUser(username, "")

	loginRes, err := s.client.VerifyUser(s.T().Context(), &pb.VerifyUserRequest{
		Username: username,
		Password: "SecurePass123!",
	})
	s.Require().NoError(err)

	rt := loginRes.RefreshToken

	res1, err := s.client.RefreshToken(s.T().Context(), &pb.RefreshTokenRequest{RefreshToken: rt})
	s.NoError(err)
	s.NotEmpty(res1.RefreshToken)

	_, err = s.client.RefreshToken(s.T().Context(), &pb.RefreshTokenRequest{RefreshToken: rt})
	s.Error(err)
	s.Equal(codes.Unauthenticated, status.Code(err))
	s.Contains(err.Error(), "reuse detected")

	_, err = s.client.RefreshToken(s.T().Context(), &pb.RefreshTokenRequest{RefreshToken: res1.RefreshToken})
	s.Error(err)
	s.Equal(codes.Unauthenticated, status.Code(err))
}

func (s *UserSuite) TestRefreshToken_Expired() {
	username := "expireduser"
	userID := s.createUser(username, "")

	tokenHash := auth.HashToken("expired-token")
	err := db.New(s.db).CreateRefreshToken(s.T().Context(), db.CreateRefreshTokenParams{
		ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
		UserID:    pgtype.UUID{Bytes: uuid.MustParse(userID), Valid: true},
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true},
	})
	s.Require().NoError(err)

	_, err = s.client.RefreshToken(s.T().Context(), &pb.RefreshTokenRequest{
		RefreshToken: "expired-token",
	})

	s.Error(err)
	s.Equal(codes.Unauthenticated, status.Code(err))
	s.Contains(err.Error(), "expired")
}

func (s *UserSuite) TestRevokeToken_Success() {
	username := "revoketest"
	s.createUser(username, "")

	loginRes, err := s.client.VerifyUser(s.T().Context(), &pb.VerifyUserRequest{
		Username: username,
		Password: "SecurePass123!",
	})
	s.Require().NoError(err)

	_, err = s.client.RevokeToken(s.T().Context(), &pb.RevokeTokenRequest{
		RefreshToken: loginRes.RefreshToken,
	})
	s.NoError(err)

	_, err = s.client.RefreshToken(s.T().Context(), &pb.RefreshTokenRequest{
		RefreshToken: loginRes.RefreshToken,
	})
	s.Error(err)
	s.Equal(codes.Unauthenticated, status.Code(err))
}

func (s *UserSuite) TestVerifyUser_IncorrectPassword() {
	username := "wrongpwuser"
	password := "SecurePass123!"

	_, err := s.client.CreateUser(s.T().Context(), &pb.CreateUserRequest{
		Username: username,
		Password: password,
	})
	s.Require().NoError(err)

	res, err := s.client.VerifyUser(s.T().Context(), &pb.VerifyUserRequest{
		Username: username,
		Password: "WrongPassword",
	})

	s.Error(err)
	s.Nil(res)
	s.Equal(codes.Unauthenticated, status.Code(err))
	s.Contains(err.Error(), "invalid username or password")
}

func (s *UserSuite) TestVerifyUser_UnknownUser() {
	_, err := s.client.VerifyUser(s.T().Context(), &pb.VerifyUserRequest{
		Username: "ghostuser",
		Password: "SecurePass123!",
	})

	s.Error(err)
	st, ok := status.FromError(err)
	s.True(ok)
	s.Equal(codes.Unauthenticated, st.Code())
	s.Contains(err.Error(), "invalid username or password")
}

func (s *UserSuite) createUser(username, password string) string {
	if password == "" {
		password = "SecurePass123!"
	}
	res, err := s.client.CreateUser(s.T().Context(), &pb.CreateUserRequest{
		Username: username,
		Password: password,
	})
	s.Require().NoError(err)
	return res.UserId
}

func (s *UserSuite) TestCreateRoom_ValidArgs() {
	aliceID := s.createUser("alicecr", "")

	roomName := "Alice Room"
	res, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      roomName,
		ManagerId: aliceID,
		Capacity:  100,
	})

	s.NoError(err)
	s.NotNil(res)
	s.NotEmpty(res.RoomId)
}

func (s *UserSuite) TestCreateRoom_InvalidValues() {
	aliceID := s.createUser("aliceinval", "")

	res, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "",
		ManagerId: aliceID,
	})

	s.Error(err)
	s.Nil(res)
	s.Equal(codes.InvalidArgument, status.Code(err))
}

func (s *UserSuite) TestListJoinedRooms_ParticipatingRooms() {
	aliceID := s.createUser("alicelr", "")
	bobID := s.createUser("boblr", "")

	roomName := "Chat Room"
	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      roomName,
		ManagerId: aliceID,
		Capacity:  100,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	res, err := s.client.ListJoinedRooms(s.T().Context(), &pb.ListJoinedRoomsRequest{
		UserId: aliceID,
	})

	s.NoError(err)
	s.Len(res.Rooms, 1)
	s.Equal(roomID, res.Rooms[0].Room.Id)
	s.Equal(roomName, res.Rooms[0].Room.Name)

	_, err = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
		UserId: bobID,
		RoomId: roomID,
	})
	s.Require().NoError(err)

	resBob, err := s.client.ListJoinedRooms(s.T().Context(), &pb.ListJoinedRoomsRequest{
		UserId: bobID,
	})

	s.NoError(err)
	s.Len(resBob.Rooms, 1)
	s.Equal(roomID, resBob.Rooms[0].Room.Id)
}

func (s *UserSuite) TestListJoinedRooms_Empty() {
	newUserID := s.createUser("newuser", "")

	res, err := s.client.ListJoinedRooms(s.T().Context(), &pb.ListJoinedRoomsRequest{
		UserId: newUserID,
	})

	s.NoError(err)
	s.NotNil(res)
	s.Empty(res.Rooms)
}

func (s *UserSuite) TestJoinRoom_NewMember() {
	aliceID := s.createUser("alicejr", "")
	charlieID := s.createUser("charliejr", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Join Test Room",
		ManagerId: aliceID,
		Capacity:  100,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	res, err := s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
		UserId: charlieID,
		RoomId: roomID,
	})

	s.NoError(err)
	s.NotNil(res)

	listRes, err := s.client.ListJoinedRooms(s.T().Context(), &pb.ListJoinedRoomsRequest{
		UserId: charlieID,
	})
	s.NoError(err)
	s.Len(listRes.Rooms, 1)
	s.Equal(roomID, listRes.Rooms[0].Room.Id)
}

func (s *UserSuite) TestJoinRoom_AlreadyMember() {
	aliceID := s.createUser("alicejrexist", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Already Member Room",
		ManagerId: aliceID,
		Capacity:  100,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	res, err := s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
		UserId: aliceID,
		RoomId: roomID,
	})

	s.NoError(err)
	s.NotNil(res)
}

func (s *UserSuite) TestJoinRoom_InvalidRoom() {
	aliceID := s.createUser("alicejrinval", "")
	fakeRoomID := "00000000-0000-0000-0000-000000000000"

	res, err := s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
		UserId: aliceID,
		RoomId: fakeRoomID,
	})

	s.Error(err)
	s.Nil(res)
}

func (s *UserSuite) TestVerifyRoomMember_Scenario() {
	aliceID := s.createUser("alicevm", "")
	bobID := s.createUser("bobvm", "")

	res, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Verify Room",
		ManagerId: aliceID,
		Capacity:  10,
	})
	s.Require().NoError(err)
	roomID := res.RoomId

	_, err = s.client.VerifyRoomMember(s.T().Context(), &pb.VerifyRoomMemberRequest{
		RoomId: roomID,
		UserId: aliceID,
	})
	s.NoError(err)

	_, err = s.client.VerifyRoomMember(s.T().Context(), &pb.VerifyRoomMemberRequest{
		RoomId: roomID,
		UserId: bobID,
	})
	s.Error(err)
	s.Equal(codes.NotFound, status.Code(err))
}

func (s *UserSuite) TestGetMemberJoinedAt_Success() {
	aliceID := s.createUser("aliceja", "")
	res, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "JoinedAt Room",
		ManagerId: aliceID,
		Capacity:  10,
	})
	s.Require().NoError(err)
	roomID := res.RoomId

	joinedRes, err := s.client.GetMemberJoinedAt(s.T().Context(), &pb.GetMemberJoinedAtRequest{
		RoomId: roomID,
		UserId: aliceID,
	})

	s.NoError(err)
	s.NotNil(joinedRes.JoinedAt)
	s.WithinDuration(time.Now(), joinedRes.JoinedAt.AsTime(), 5*time.Second)
}

func (s *UserSuite) TestJoinRoom_CapacityExceeded() {
	aliceID := s.createUser("alicecap", "")
	bobID := s.createUser("bobcap", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Small Room",
		ManagerId: aliceID,
		Capacity:  1,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	res, err := s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
		UserId: bobID,
		RoomId: roomID,
	})

	s.Error(err)
	s.Nil(res)
	s.Equal(codes.FailedPrecondition, status.Code(err))
}

func (s *UserSuite) TestSearchRooms() {
	mgrID := s.createUser("manager1", "")
	s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{Name: "Golang Room", ManagerId: mgrID, Capacity: 100})
	s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{Name: "Python Room", ManagerId: mgrID, Capacity: 100})
	s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{Name: "Go Chat", ManagerId: mgrID, Capacity: 100})

	res, err := s.client.SearchRooms(s.T().Context(), &pb.SearchRoomsRequest{
		Query: "Go", Limit: 10, Offset: 0,
	})

	s.Require().NoError(err)
	s.Require().NotNil(res)
	s.Len(res.Rooms, 2)
	s.True(res.TotalCount >= 2)
}

func (s *UserSuite) TestUpdateRoom_ManagerOnly() {
	aliceID := s.createUser("alicemgr", "")
	bobID := s.createUser("bobnonmgr", "")
	res, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Old Name",
		ManagerId: aliceID,
		Capacity:  100,
	})
	s.Require().NoError(err)
	roomID := res.RoomId

	updateRes, err := s.client.UpdateRoom(s.T().Context(), &pb.UpdateRoomRequest{
		Id:          roomID,
		Name:        "New Name",
		Capacity:    200,
		RequesterId: aliceID,
	})
	s.NoError(err)
	s.NotNil(updateRes)

	name, capacity, _ := s.getRoom(roomID)
	s.Equal("New Name", name)
	s.Equal(int32(200), capacity)

	_, err = s.client.UpdateRoom(s.T().Context(), &pb.UpdateRoomRequest{
		Id:          roomID,
		Name:        "Hack Name",
		Capacity:    999,
		RequesterId: bobID,
	})

	s.Error(err)
	s.Equal(codes.PermissionDenied, status.Code(err))
}

func (s *UserSuite) TestLeaveRoom_ManagerDelegation() {
	aliceID := s.createUser("leavealice", "")
	bobID := s.createUser("leavebob", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Leave Test Room",
		ManagerId: aliceID,
		Capacity:  100,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	_, err = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
		UserId: bobID,
		RoomId: roomID,
	})
	s.Require().NoError(err)

	leaveRes, err := s.client.LeaveRoom(s.T().Context(), &pb.LeaveRoomRequest{
		RoomId: roomID,
		UserId: aliceID,
	})

	s.NoError(err)
	s.NotNil(leaveRes)

	_, _, managerID := s.getRoom(roomID)
	s.Equal(bobID, managerID)

	leaveRes2, err := s.client.LeaveRoom(s.T().Context(), &pb.LeaveRoomRequest{
		RoomId: roomID,
		UserId: bobID,
	})
	s.NoError(err)
	s.NotNil(leaveRes2)

	s.False(s.roomExists(roomID))
}

func (s *UserSuite) TestCreateUser_TimestampSync() {
	req := &pb.CreateUserRequest{
		Username: "tsuser",
		Password: "SecurePassword123!",
	}

	before := time.Now()
	res, err := s.client.CreateUser(s.T().Context(), req)
	after := time.Now()

	s.Require().NoError(err)
	s.Require().NotEmpty(res.UserId)

	userIDParsed, err := uuid.Parse(res.UserId)
	s.Require().NoError(err)

	var createdAt time.Time
	err = s.db.QueryRow(s.T().Context(), "SELECT created_at FROM users WHERE id = $1", res.UserId).Scan(&createdAt)
	s.Require().NoError(err)

	sec, nsec := userIDParsed.Time().UnixTime()
	uuidTime := time.Unix(sec, nsec).In(time.UTC)
	dbTime := createdAt.In(time.UTC)

	s.Equal(uuidTime.UnixMilli(), dbTime.UnixMilli(), "UUID timestamp and DB created_at should match in milliseconds")

	s.True(dbTime.After(before.Add(-1 * time.Second)))
	s.True(dbTime.Before(after.Add(1 * time.Second)))
}

func (s *UserSuite) TestDeleteRoom_ManagerOnly() {
	aliceID := s.createUser("deletealice", "")
	bobID := s.createUser("deletebob", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Delete Test Room",
		ManagerId: aliceID,
		Capacity:  100,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	_, err = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
		UserId: bobID,
		RoomId: roomID,
	})
	s.Require().NoError(err)

	deleteRes, err := s.client.DeleteRoom(s.T().Context(), &pb.DeleteRoomRequest{
		RoomId:      roomID,
		RequesterId: bobID,
	})

	s.Error(err)
	s.Nil(deleteRes)
	s.Equal(codes.PermissionDenied, status.Code(err))

	s.True(s.roomExists(roomID), "room should still exist after non-manager delete attempt")

	deleteRes, err = s.client.DeleteRoom(s.T().Context(), &pb.DeleteRoomRequest{
		RoomId:      roomID,
		RequesterId: aliceID,
	})
	s.NoError(err)
	s.NotNil(deleteRes)

	s.False(s.roomExists(roomID), "room should not exist after manager delete")
}

func (s *UserSuite) TestDeleteRoom_NotFoundAfterDelete() {
	aliceID := s.createUser("deletenfound", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "ToBeDeleted Room",
		ManagerId: aliceID,
		Capacity:  100,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	_, err = s.client.DeleteRoom(s.T().Context(), &pb.DeleteRoomRequest{
		RoomId:      roomID,
		RequesterId: aliceID,
	})
	s.Require().NoError(err)

	listRes, err := s.client.ListJoinedRooms(s.T().Context(), &pb.ListJoinedRoomsRequest{
		UserId: aliceID,
	})
	s.NoError(err)
	for _, ur := range listRes.Rooms {
		s.NotEqual(roomID, ur.Room.Id, "Deleted room should not appear in list")
	}

	searchRes, err := s.client.SearchRooms(s.T().Context(), &pb.SearchRoomsRequest{
		Query: "ToBeDeleted", Limit: 10, Offset: 0,
	})
	s.NoError(err)
	for _, room := range searchRes.Rooms {
		s.NotEqual(roomID, room.Id, "Deleted room should not appear in search")
	}
}

func (s *UserSuite) TestJoinRoom_ConcurrentCapacityEnforcement() {
	managerID := s.createUser("racemanager", "")
	room, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name: "Race Room", ManagerId: managerID, Capacity: 3,
	})
	s.Require().NoError(err)

	const numJoiners = 5
	userIDs := make([]string, numJoiners)
	for i := range numJoiners {
		userIDs[i] = s.createUser(fmt.Sprintf("racer%d", i), "")
	}

	var wg sync.WaitGroup
	results := make([]error, numJoiners)
	for i := range numJoiners {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, results[idx] = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
				UserId: userIDs[idx], RoomId: room.RoomId,
			})
		}(i)
	}
	wg.Wait()

	successCount, fullCount := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successCount++
		case status.Code(err) == codes.FailedPrecondition:
			fullCount++
		}
	}

	s.Equal(2, successCount, "capacity=3, manager=1 slot -> 2 joins succeed")
	s.Equal(3, fullCount, "나머지는 FailedPrecondition")
}

func (s *UserSuite) TestRefreshToken_ConcurrentReuseDetection() {
	username := "refreshconc"
	s.createUser(username, "")

	loginRes, err := s.client.VerifyUser(s.T().Context(), &pb.VerifyUserRequest{
		Username: username,
		Password: "SecurePass123!",
	})
	s.Require().NoError(err)

	rt := loginRes.RefreshToken
	const numRequests = 5

	var wg sync.WaitGroup
	results := make([]*pb.RefreshTokenResponse, numRequests)
	errs := make([]error, numRequests)
	for i := range numRequests {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = s.client.RefreshToken(s.T().Context(), &pb.RefreshTokenRequest{
				RefreshToken: rt,
			})
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}

	s.Equal(1, successCount, "동일 토큰 동시 사용 시 정확히 1건만 성공해야 함")
}

func (s *UserSuite) TestUpdateRoom_CapacityBelowMemberCount() {
	aliceID := s.createUser("capalice", "")
	bobID := s.createUser("capbob", "")
	charlieID := s.createUser("capcharlie", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Capacity Shrink Room",
		ManagerId: aliceID,
		Capacity:  10,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	for _, uid := range []string{bobID, charlieID} {
		_, err = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
			UserId: uid,
			RoomId: roomID,
		})
		s.Require().NoError(err)
	}

	_, err = s.client.UpdateRoom(s.T().Context(), &pb.UpdateRoomRequest{
		Id:          roomID,
		Name:        "Shrunk Room",
		Capacity:    2,
		RequesterId: aliceID,
	})
	s.Error(err)
	s.Equal(codes.FailedPrecondition, status.Code(err))

	_, err = s.client.UpdateRoom(s.T().Context(), &pb.UpdateRoomRequest{
		Id:          roomID,
		Name:        "Shrunk Room",
		Capacity:    3,
		RequesterId: aliceID,
	})
	s.NoError(err)

	_, capacity, _ := s.getRoom(roomID)
	s.Equal(int32(3), capacity)
}

func (s *UserSuite) TestUpdateRoom_ConcurrentWithJoinRoom() {
	managerID := s.createUser("updatemgr", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Update Race Room",
		ManagerId: managerID,
		Capacity:  10,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	const numJoiners = 5
	userIDs := make([]string, numJoiners)
	for i := range numJoiners {
		userIDs[i] = s.createUser(fmt.Sprintf("updjoiner%d", i), "")
	}

	var wg sync.WaitGroup
	joinErrs := make([]error, numJoiners)
	for i := range numJoiners {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, joinErrs[idx] = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
				UserId: userIDs[idx], RoomId: roomID,
			})
		}(i)
	}

	wg.Add(1)
	var updateErr error
	go func() {
		defer wg.Done()
		_, updateErr = s.client.UpdateRoom(s.T().Context(), &pb.UpdateRoomRequest{
			Id: roomID, Name: "Shrunk", Capacity: 3, RequesterId: managerID,
		})
	}()
	wg.Wait()

	var memberCount int64
	err = s.db.QueryRow(s.T().Context(),
		"SELECT COUNT(*) FROM room_members WHERE room_id = $1", roomID,
	).Scan(&memberCount)
	s.Require().NoError(err)

	_, capacity, _ := s.getRoom(roomID)

	if updateErr == nil {

		s.Equal(int32(3), capacity)
		s.LessOrEqual(memberCount, int64(3))
	} else {

		s.Equal(int32(10), capacity)
	}

	s.LessOrEqual(memberCount, int64(capacity),
		"member count must not exceed capacity (members=%d, capacity=%d)", memberCount, capacity)

	joinSuccess := 0
	for _, e := range joinErrs {
		if e == nil {
			joinSuccess++
		}
	}
	s.T().Logf("join success=%d, updateErr=%v, members=%d, capacity=%d",
		joinSuccess, updateErr, memberCount, capacity)
}

func (s *UserSuite) TestDeleteRoom_ConcurrentWithJoinRoom() {
	managerID := s.createUser("delmgr", "")
	joinerID := s.createUser("deljoiner", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Delete Race Room",
		ManagerId: managerID,
		Capacity:  10,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	var wg sync.WaitGroup
	var joinErr, deleteErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, joinErr = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
			UserId: joinerID, RoomId: roomID,
		})
	}()
	go func() {
		defer wg.Done()
		_, deleteErr = s.client.DeleteRoom(s.T().Context(), &pb.DeleteRoomRequest{
			RoomId: roomID, RequesterId: managerID,
		})
	}()
	wg.Wait()

	if deleteErr == nil {
		s.False(s.roomExists(roomID), "삭제된 방은 존재하지 않아야 함")
	}
	if joinErr == nil && deleteErr == nil {

		s.False(s.roomExists(roomID))
	}
	if joinErr != nil {

		s.Equal(codes.NotFound, status.Code(joinErr))
	}

	s.T().Logf("joinErr=%v, deleteErr=%v", joinErr, deleteErr)
}

func (s *UserSuite) TestLeaveRoom_ConcurrentManagerDelegation() {
	aliceID := s.createUser("leavealice2", "")
	bobID := s.createUser("leavebob2", "")
	charlieID := s.createUser("leavecharl2", "")

	createRes, err := s.client.CreateRoom(s.T().Context(), &pb.CreateRoomRequest{
		Name:      "Concurrent Leave Room",
		ManagerId: aliceID,
		Capacity:  10,
	})
	s.Require().NoError(err)
	roomID := createRes.RoomId

	for _, uid := range []string{bobID, charlieID} {
		_, err = s.client.JoinRoom(s.T().Context(), &pb.JoinRoomRequest{
			UserId: uid,
			RoomId: roomID,
		})
		s.Require().NoError(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = s.client.LeaveRoom(s.T().Context(), &pb.LeaveRoomRequest{
			RoomId: roomID, UserId: aliceID,
		})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = s.client.LeaveRoom(s.T().Context(), &pb.LeaveRoomRequest{
			RoomId: roomID, UserId: bobID,
		})
	}()
	wg.Wait()

	s.NoError(errs[0])
	s.NoError(errs[1])

	_, _, managerID := s.getRoom(roomID)
	s.NotEmpty(managerID, "방장이 반드시 지정되어야 함")
	s.Equal(charlieID, managerID, "유일한 잔존 멤버 charlie가 방장이어야 함")
}

func (s *UserSuite) getRoom(roomID string) (string, int32, string) {
	var name string
	var capacity int32
	var managerID *string
	err := s.db.QueryRow(s.T().Context(),
		"SELECT name, capacity, manager_id::text FROM rooms WHERE id = $1 AND deleted_at IS NULL",
		roomID,
	).Scan(&name, &capacity, &managerID)
	s.Require().NoError(err)
	if managerID == nil {
		return name, capacity, ""
	}
	return name, capacity, *managerID
}

func (s *UserSuite) roomExists(roomID string) bool {
	var exists bool
	err := s.db.QueryRow(s.T().Context(),
		"SELECT EXISTS(SELECT 1 FROM rooms WHERE id = $1 AND deleted_at IS NULL)",
		roomID,
	).Scan(&exists)
	s.Require().NoError(err)
	return exists
}

func TestUserSuite(t *testing.T) {
	suite.Run(t, new(UserSuite))
}
