package user

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/crypto/bcrypt"

	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/auth"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/user/db"
	"go-chat-msa/internal/user/hasher"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	pb.UnsafeUserServiceServer
	config    config.UserConfig
	secretKey string
	queries   db.Querier
	hasher    *hasher.Pool
	tokens    RefreshTokenStore
	runInTx   func(ctx context.Context, fn func(db.Querier) error) error
}

func NewService(dbConn db.Querier, cfg config.UserConfig, secretKey string, h *hasher.Pool) *Service {
	return &Service{
		queries:   dbConn,
		config:    cfg,
		secretKey: secretKey,
		hasher:    h,
		tokens:    missingRefreshTokenStore{},

		runInTx: func(ctx context.Context, fn func(db.Querier) error) error {
			return fn(dbConn)
		},
	}
}

func (s *Service) WithRunInTx(runInTx func(ctx context.Context, fn func(db.Querier) error) error) *Service {
	s.runInTx = runInTx
	return s
}

func (s *Service) WithRefreshTokenStore(tokens RefreshTokenStore) *Service {
	if tokens == nil {
		s.tokens = missingRefreshTokenStore{}
		return s
	}
	s.tokens = tokens
	return s
}

func (s *Service) CreateUser(ctx context.Context, req *pb.CreateUserRequest) (*pb.CreateUserResponse, error) {
	if err := validateUsername(req.Username, s.config); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validatePassword(req.Password, s.config); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	hashedPassword, err := s.hasher.HashPassword(ctx, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, hasher.ErrQueueFull):
			return nil, status.Error(codes.ResourceExhausted, "system overloaded")
		case errors.Is(err, hasher.ErrClosed):
			return nil, status.Error(codes.Unavailable, "service shutting down")
		default:
			slog.ErrorContext(ctx, "failed to hash password", "error", err)
			return nil, status.Error(codes.Internal, "failed to hash password")
		}
	}

	userID, err := uuid.NewV7()
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to generate user ID")
	}
	createdAt := time.Unix(userID.Time().UnixTime())

	arg := db.CreateUserParams{
		ID:           pgtype.UUID{Bytes: userID, Valid: true},
		Username:     req.Username,
		PasswordHash: string(hashedPassword),
		CreatedAt:    pgtype.Timestamptz{Time: createdAt, Valid: true},
	}

	user, err := s.queries.CreateUser(ctx, arg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			userCreatedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			return nil, status.Error(codes.AlreadyExists, "username already exists")
		}
		slog.ErrorContext(ctx, "failed to create user", "error", err)
		userCreatedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return nil, status.Error(codes.Internal, "failed to create user")
	}

	userCreatedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
	return &pb.CreateUserResponse{UserId: user.ID.String()}, nil
}

func (s *Service) VerifyUser(ctx context.Context, req *pb.VerifyUserRequest) (*pb.VerifyUserResponse, error) {
	user, err := s.queries.GetUserByUsername(ctx, req.Username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.Unauthenticated, "invalid username or password")
		}
		slog.ErrorContext(ctx, "failed to get user", "error", err)
		return nil, status.Error(codes.Internal, "failed to get user")
	}

	if err := s.hasher.ComparePassword(ctx, user.PasswordHash, req.Password); err != nil {
		switch {
		case errors.Is(err, hasher.ErrQueueFull):
			return nil, status.Error(codes.ResourceExhausted, "system overloaded")
		case errors.Is(err, hasher.ErrClosed):
			return nil, status.Error(codes.Unavailable, "service shutting down")
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			return nil, status.Error(codes.Unauthenticated, "invalid username or password")
		default:
			slog.ErrorContext(ctx, "failed to compare password", "error", err)
			authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			return nil, status.Error(codes.Internal, "failed to verify user")
		}
	}

	accessToken, refreshToken, err := s.issueTokenPair(ctx, user.ID, user.Username)
	if err != nil {
		return nil, err
	}

	authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
	return &pb.VerifyUserResponse{
		UserId:       user.ID.String(),
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *Service) RefreshToken(ctx context.Context, req *pb.RefreshTokenRequest) (*pb.RefreshTokenResponse, error) {
	validation, err := s.tokens.Validate(ctx, req.RefreshToken)
	if err != nil {
		slog.ErrorContext(ctx, "failed to validate refresh token", "error", err)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	switch validation.Status {
	case RefreshTokenValidationInvalid:
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	case RefreshTokenValidationReused:
		authTokenReuseTotal.Add(ctx, 1)
		slog.WarnContext(ctx, "refresh token reuse detected, revoking all tokens", "user_id", validation.UserID)
		return nil, status.Error(codes.Unauthenticated, "refresh token reuse detected")
	case RefreshTokenValidationActive:
	default:
		slog.ErrorContext(ctx, "unexpected refresh token validation status", "status", validation.Status)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	userUUID, err := toPGUUID(validation.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "invalid user id in refresh token store", "user_id", validation.UserID, "error", err)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	user, err := s.queries.GetUserByID(ctx, userUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if revokeErr := s.tokens.RevokeUser(ctx, validation.UserID); revokeErr != nil {
				slog.WarnContext(ctx, "failed to revoke refresh tokens for missing user", "user_id", validation.UserID, "error", revokeErr)
			}
			return nil, status.Error(codes.Unauthenticated, "user no longer exists")
		}
		slog.ErrorContext(ctx, "failed to get user for refresh", "error", err)
		return nil, status.Error(codes.Internal, "failed to get user")
	}

	accessToken, err := s.issueAccessToken(user.ID, user.Username)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate access token", "error", err)
		return nil, status.Error(codes.Internal, "failed to generate access token")
	}

	refreshToken := uuid.NewString()
	rotation, err := s.tokens.Rotate(ctx, req.RefreshToken, refreshToken, s.refreshTokenTTL())
	if err != nil {
		slog.ErrorContext(ctx, "failed to rotate refresh token", "error", err)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	switch rotation.Status {
	case RefreshTokenInvalid:
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	case RefreshTokenReused:
		authTokenReuseTotal.Add(ctx, 1)
		slog.WarnContext(ctx, "refresh token reuse detected, revoking all tokens", "user_id", rotation.UserID)
		return nil, status.Error(codes.Unauthenticated, "refresh token reuse detected")
	case RefreshTokenRotated:
		if rotation.UserID != validation.UserID {
			slog.ErrorContext(ctx, "refresh token owner changed during rotation",
				"validated_user_id", validation.UserID,
				"rotated_user_id", rotation.UserID,
			)
			return nil, status.Error(codes.Internal, "failed to refresh token")
		}
	default:
		slog.ErrorContext(ctx, "unexpected refresh token rotation status", "status", rotation.Status)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	return &pb.RefreshTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *Service) RevokeToken(ctx context.Context, req *pb.RevokeTokenRequest) (*pb.RevokeTokenResponse, error) {
	if err := s.tokens.Revoke(ctx, req.RefreshToken); err != nil {
		slog.ErrorContext(ctx, "failed to revoke token", "error", err)
		return nil, status.Error(codes.Internal, "failed to revoke token")
	}

	return &pb.RevokeTokenResponse{}, nil
}

func (s *Service) BatchGetUsers(ctx context.Context, req *pb.BatchGetUsersRequest) (*pb.BatchGetUsersResponse, error) {
	if len(req.UserIds) == 0 {
		return &pb.BatchGetUsersResponse{}, nil
	}

	uuids := make([]pgtype.UUID, 0, len(req.UserIds))
	for _, id := range req.UserIds {
		uid, err := toPGUUID(id)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid user_id: %s", id)
		}
		uuids = append(uuids, uid)
	}

	rows, err := s.queries.GetUsersByIDs(ctx, uuids)
	if err != nil {
		slog.ErrorContext(ctx, "failed to batch get users", "error", err)
		return nil, status.Error(codes.Internal, "failed to batch get users")
	}

	users := make([]*pb.User, 0, len(rows))
	for _, row := range rows {
		users = append(users, &pb.User{
			Id:        row.ID.String(),
			Username:  row.Username,
			CreatedAt: timestamppb.New(row.CreatedAt.Time),
		})
	}
	return &pb.BatchGetUsersResponse{Users: users}, nil
}

func (s *Service) issueAccessToken(userID pgtype.UUID, username string) (string, error) {
	accessTokenDuration := time.Duration(s.config.Token.AccessTokenExpirationMinutes) * time.Minute
	accessToken, err := auth.GenerateJWT(userID.String(), username, s.secretKey, accessTokenDuration)
	if err != nil {
		return "", err
	}
	return accessToken, nil
}

func (s *Service) issueTokenPair(ctx context.Context, userID pgtype.UUID, username string) (string, string, error) {
	accessToken, err := s.issueAccessToken(userID, username)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate access token", "error", err)
		return "", "", status.Error(codes.Internal, "failed to generate access token")
	}
	refreshToken := uuid.NewString()

	if err := s.tokens.Issue(ctx, userID.String(), refreshToken, s.refreshTokenTTL()); err != nil {
		slog.ErrorContext(ctx, "failed to save refresh token", "error", err)
		return "", "", status.Error(codes.Internal, "failed to save refresh token")
	}

	return accessToken, refreshToken, nil
}

func (s *Service) refreshTokenTTL() time.Duration {
	return time.Duration(s.config.Token.RefreshTokenExpirationDays) * 24 * time.Hour
}

func (s *Service) CreateRoom(ctx context.Context, req *pb.CreateRoomRequest) (*pb.CreateRoomResponse, error) {
	if err := validateRoomName(req.Name, s.config.Room.MaxNameLength); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	managerUUID, err := toPGUUID(req.ManagerId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid manager_id")
	}

	roomID, err := uuid.NewV7()
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to generate room ID")
	}
	createdAt := time.Unix(roomID.Time().UnixTime())

	if err := validateCapacity(req.Capacity, s.config.Room.MaxCapacity); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	var roomIDStr string
	if err := s.runInTx(ctx, func(qtx db.Querier) error {
		room, err := qtx.CreateRoom(ctx, db.CreateRoomParams{
			ID:        pgtype.UUID{Bytes: roomID, Valid: true},
			Name:      req.Name,
			ManagerID: managerUUID,
			Capacity:  req.Capacity,
			CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed to create room", "error", err)
			return status.Error(codes.Internal, "failed to create room")
		}

		if err := qtx.CreateRoomMember(ctx, db.CreateRoomMemberParams{
			UserID:   managerUUID,
			RoomID:   room.ID,
			JoinedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
		}); err != nil {
			slog.ErrorContext(ctx, "failed to add manager to room members", "error", err)
			return status.Error(codes.Internal, "failed to join room as manager")
		}

		roomIDStr = room.ID.String()
		return nil
	}); err != nil {
		return nil, err
	}

	return &pb.CreateRoomResponse{RoomId: roomIDStr}, nil
}

func (s *Service) GetMemberJoinedAt(ctx context.Context, req *pb.GetMemberJoinedAtRequest) (*pb.GetMemberJoinedAtResponse, error) {
	roomUUID, err := toPGUUID(req.RoomId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid room_id")
	}
	userUUID, err := toPGUUID(req.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid user_id")
	}

	joinedAt, err := s.queries.GetMemberJoinedAt(ctx, db.GetMemberJoinedAtParams{
		RoomID: roomUUID,
		UserID: userUUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not a member of the room")
		}
		slog.ErrorContext(ctx, "failed to get member joined_at", "error", err)
		return nil, status.Error(codes.Internal, "failed to get member joined_at")
	}

	return &pb.GetMemberJoinedAtResponse{
		JoinedAt: timestamppb.New(joinedAt.Time),
	}, nil
}

func (s *Service) VerifyRoomMember(ctx context.Context, req *pb.VerifyRoomMemberRequest) (*pb.VerifyRoomMemberResponse, error) {
	roomUUID, err := toPGUUID(req.RoomId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid room_id")
	}
	userUUID, err := toPGUUID(req.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid user_id")
	}

	exists, err := s.queries.ExistsRoomMember(ctx, db.ExistsRoomMemberParams{
		RoomID: roomUUID,
		UserID: userUUID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to verify room member", "error", err)
		return nil, status.Error(codes.Internal, "failed to verify room member")
	}
	if !exists {
		return nil, status.Error(codes.NotFound, "not a member of the room")
	}

	return &pb.VerifyRoomMemberResponse{}, nil
}

func (s *Service) ListJoinedRooms(ctx context.Context, req *pb.ListJoinedRoomsRequest) (*pb.ListJoinedRoomsResponse, error) {
	userUUID, err := toPGUUID(req.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid user_id")
	}

	rows, err := s.queries.ListJoinedRooms(ctx, userUUID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list joined rooms", "error", err)
		return nil, status.Error(codes.Internal, "failed to list joined rooms")
	}

	return &pb.ListJoinedRoomsResponse{Rooms: userRoomsFromRows(rows)}, nil
}

func (s *Service) ListRoomMembers(ctx context.Context, req *pb.ListRoomMembersRequest) (*pb.ListRoomMembersResponse, error) {
	roomUUID, err := toPGUUID(req.RoomId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid room_id")
	}

	rows, err := s.queries.ListRoomMembers(ctx, roomUUID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list room members", "error", err)
		return nil, status.Error(codes.Internal, "failed to list room members")
	}

	return &pb.ListRoomMembersResponse{Members: roomMembersFromRows(rows)}, nil
}

func (s *Service) JoinRoom(ctx context.Context, req *pb.JoinRoomRequest) (*pb.JoinRoomResponse, error) {
	userUUID, err := toPGUUID(req.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid user_id")
	}
	roomUUID, err := toPGUUID(req.RoomId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid room_id")
	}

	if err := s.runInTx(ctx, func(qtx db.Querier) error {
		room, err := qtx.GetRoomForUpdate(ctx, roomUUID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return status.Error(codes.NotFound, "room not found")
			}
			return err
		}

		exists, err := qtx.ExistsRoomMember(ctx, db.ExistsRoomMemberParams{RoomID: roomUUID, UserID: userUUID})
		if err != nil {
			return err
		}
		if exists {
			return nil
		}

		count, err := qtx.GetRoomMemberCount(ctx, roomUUID)
		if err != nil {
			return err
		}

		if int32(count) >= room.Capacity {
			roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			return status.Error(codes.FailedPrecondition, "room is full")
		}

		if err := qtx.CreateRoomMember(ctx, db.CreateRoomMemberParams{
			UserID:   userUUID,
			RoomID:   roomUUID,
			JoinedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		st, ok := status.FromError(err)
		if ok {
			switch st.Code() {
			case codes.NotFound:
				roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			case codes.FailedPrecondition:
			default:
				roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			}
			return nil, err
		}
		slog.ErrorContext(ctx, "failed to join room", "error", err)
		roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return nil, status.Error(codes.Internal, "failed to join room")
	}

	roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
	return &pb.JoinRoomResponse{}, nil
}

func (s *Service) SearchRooms(ctx context.Context, req *pb.SearchRoomsRequest) (*pb.SearchRoomsResponse, error) {
	limit := req.Limit
	if limit > s.config.Search.MaxLimit {
		return nil, status.Error(codes.InvalidArgument, "limit exceeds maximum allowed")
	}
	if limit <= 0 {
		limit = s.config.Search.DefaultLimit
	}

	rows, err := s.queries.SearchRooms(ctx, db.SearchRoomsParams{
		Column1: pgtype.Text{String: req.Query, Valid: true},
		Limit:   limit,
		Offset:  req.Offset,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to search rooms", "error", err)
		return nil, status.Error(codes.Internal, "failed to search rooms")
	}

	var totalCount int32
	if len(rows) > 0 {
		totalCount = int32(rows[0].TotalCount)
	}

	return &pb.SearchRoomsResponse{Rooms: roomsFromSearchRows(rows), TotalCount: totalCount}, nil
}

func (s *Service) UpdateRoom(ctx context.Context, req *pb.UpdateRoomRequest) (*pb.UpdateRoomResponse, error) {
	roomUUID, err := toPGUUID(req.Id)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid room_id")
	}
	requesterUUID, err := toPGUUID(req.RequesterId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid requester_id")
	}

	if err := validateRoomName(req.Name, s.config.Room.MaxNameLength); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateCapacity(req.Capacity, s.config.Room.MaxCapacity); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := s.runInTx(ctx, func(qtx db.Querier) error {
		room, err := qtx.GetRoomForUpdate(ctx, roomUUID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return status.Error(codes.NotFound, "room not found")
			}
			return status.Error(codes.Internal, "failed to get room")
		}

		if !room.ManagerID.Valid || room.ManagerID.Bytes != requesterUUID.Bytes {
			return status.Error(codes.PermissionDenied, "only manager can update room")
		}

		count, err := qtx.GetRoomMemberCount(ctx, roomUUID)
		if err != nil {
			return status.Error(codes.Internal, "failed to get member count")
		}
		if int32(count) > req.Capacity {
			return status.Error(codes.FailedPrecondition, "capacity cannot be less than current member count")
		}

		if _, err := qtx.UpdateRoom(ctx, db.UpdateRoomParams{
			ID:        roomUUID,
			Name:      req.Name,
			Capacity:  req.Capacity,
			ManagerID: requesterUUID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed to update room", "error", err)
			return status.Error(codes.Internal, "failed to update room")
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return &pb.UpdateRoomResponse{}, nil
}

func (s *Service) LeaveRoom(ctx context.Context, req *pb.LeaveRoomRequest) (*pb.LeaveRoomResponse, error) {
	roomUUID, err := toPGUUID(req.RoomId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid room_id")
	}
	userUUID, err := toPGUUID(req.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid user_id")
	}

	if err := s.runInTx(ctx, func(qtx db.Querier) error {
		return s.leaveRoomTx(ctx, qtx, roomUUID, userUUID)
	}); err != nil {
		return nil, err
	}

	return &pb.LeaveRoomResponse{}, nil
}

func (s *Service) leaveRoomTx(ctx context.Context, qtx db.Querier, roomUUID, userUUID pgtype.UUID) error {
	room, err := qtx.GetRoomForUpdate(ctx, roomUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.NotFound, "room not found")
		}
		return status.Error(codes.Internal, "failed to get room")
	}

	if err := qtx.DeleteRoomMember(ctx, db.DeleteRoomMemberParams{RoomID: roomUUID, UserID: userUUID}); err != nil {
		slog.ErrorContext(ctx, "failed to delete room member", "error", err)
		return status.Error(codes.Internal, "failed to leave room")
	}

	if !(room.ManagerID.Valid && room.ManagerID.Bytes == userUUID.Bytes) {
		return nil
	}

	oldestMember, err := qtx.GetOldestRoomMember(ctx, roomUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := qtx.DeleteRoom(ctx, db.DeleteRoomParams{
			ID:        roomUUID,
			ManagerID: room.ManagerID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed to delete empty room", "error", err)
			return status.Error(codes.Internal, "failed to delete empty room")
		}
		return nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to get oldest member", "error", err)
		return status.Error(codes.Internal, "failed to delegate room manager")
	}
	if err := qtx.UpdateRoomManager(ctx, db.UpdateRoomManagerParams{ID: roomUUID, ManagerID: oldestMember}); err != nil {
		slog.ErrorContext(ctx, "failed to delegate room manager", "error", err, "room_id", roomUUID)
		return status.Error(codes.Internal, "failed to delegate room manager")
	}
	return nil
}

func (s *Service) DeleteUser(ctx context.Context, req *pb.DeleteUserRequest) (*pb.DeleteUserResponse, error) {
	userUUID, err := toPGUUID(req.UserId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid user_id")
	}

	user, err := s.queries.GetUserByID(ctx, userUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			userDeletedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			return nil, status.Error(codes.NotFound, "user not found")
		}
		slog.ErrorContext(ctx, "failed to get user", "error", err)
		userDeletedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return nil, status.Error(codes.Internal, "failed to get user")
	}

	if err := s.hasher.ComparePassword(ctx, user.PasswordHash, req.Password); err != nil {
		userDeletedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		switch {
		case errors.Is(err, hasher.ErrQueueFull):
			return nil, status.Error(codes.ResourceExhausted, "system overloaded")
		case errors.Is(err, hasher.ErrClosed):
			return nil, status.Error(codes.Unavailable, "service shutting down")
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			return nil, status.Error(codes.Unauthenticated, "invalid password")
		default:
			slog.ErrorContext(ctx, "failed to compare password", "error", err)
			return nil, status.Error(codes.Internal, "failed to verify password")
		}
	}

	if err := s.tokens.RevokeUser(ctx, userUUID.String()); err != nil {
		slog.ErrorContext(ctx, "failed to revoke user refresh tokens", "user_id", userUUID.String(), "error", err)
		userDeletedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return nil, status.Error(codes.Internal, "failed to revoke tokens")
	}

	var leftRoomIDs []string
	if err := s.runInTx(ctx, func(qtx db.Querier) error {
		roomIDs, err := qtx.ListJoinedRoomIDsForUpdate(ctx, userUUID)
		if err != nil {
			slog.ErrorContext(ctx, "failed to list joined rooms", "error", err)
			return status.Error(codes.Internal, "failed to list joined rooms")
		}

		leftRoomIDs = make([]string, 0, len(roomIDs))
		for _, roomID := range roomIDs {
			if err := s.leaveRoomTx(ctx, qtx, roomID, userUUID); err != nil {
				return err
			}
			leftRoomIDs = append(leftRoomIDs, roomID.String())
		}

		if _, err := qtx.DeleteUser(ctx, userUUID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return status.Error(codes.NotFound, "user not found")
			}
			slog.ErrorContext(ctx, "failed to delete user", "error", err)
			return status.Error(codes.Internal, "failed to delete user")
		}
		return nil
	}); err != nil {
		userDeletedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return nil, err
	}

	userDeletedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
	return &pb.DeleteUserResponse{LeftRoomIds: leftRoomIDs}, nil
}

func (s *Service) DeleteRoom(ctx context.Context, req *pb.DeleteRoomRequest) (*pb.DeleteRoomResponse, error) {
	roomUUID, err := toPGUUID(req.RoomId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid room_id")
	}
	requesterUUID, err := toPGUUID(req.RequesterId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid requester_id")
	}

	if err := s.runInTx(ctx, func(qtx db.Querier) error {
		room, err := qtx.GetRoomForUpdate(ctx, roomUUID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return status.Error(codes.NotFound, "room not found")
			}
			return status.Error(codes.Internal, "failed to get room")
		}

		if !room.ManagerID.Valid || room.ManagerID.Bytes != requesterUUID.Bytes {
			return status.Error(codes.PermissionDenied, "only manager can delete room")
		}

		if _, err := qtx.DeleteRoom(ctx, db.DeleteRoomParams{
			ID:        roomUUID,
			ManagerID: requesterUUID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed to delete room", "error", err)
			return status.Error(codes.Internal, "failed to delete room")
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return &pb.DeleteRoomResponse{}, nil
}
