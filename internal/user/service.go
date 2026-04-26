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
	runInTx   func(ctx context.Context, fn func(db.Querier) error) error
}

func NewService(dbConn db.Querier, cfg config.UserConfig, secretKey string, h *hasher.Pool) *Service {
	return &Service{
		queries:   dbConn,
		config:    cfg,
		secretKey: secretKey,
		hasher:    h,

		runInTx: func(ctx context.Context, fn func(db.Querier) error) error {
			return fn(dbConn)
		},
	}
}

func (s *Service) WithRunInTx(runInTx func(ctx context.Context, fn func(db.Querier) error) error) *Service {
	s.runInTx = runInTx
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
			userCreatedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "already_exists")))
			return nil, status.Error(codes.AlreadyExists, "username already exists")
		}
		slog.ErrorContext(ctx, "failed to create user", "error", err)
		userCreatedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "internal")))
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
			authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "invalid_credentials")))
			return nil, status.Error(codes.Unauthenticated, "invalid username or password")
		default:
			slog.ErrorContext(ctx, "failed to compare password", "error", err)
			authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "internal")))
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
	tokenHash := auth.HashToken(req.RefreshToken)

	var accessToken, refreshToken string
	var reuseDetected bool
	if err := s.runInTx(ctx, func(qtx db.Querier) error {
		rt, err := qtx.GetRefreshTokenByHashForUpdate(ctx, tokenHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return status.Error(codes.Unauthenticated, "invalid refresh token")
			}
			slog.ErrorContext(ctx, "failed to get refresh token", "error", err)
			return status.Error(codes.Internal, "failed to verify refresh token")
		}

		if rt.ExpiresAt.Time.Before(time.Now()) {
			return status.Error(codes.Unauthenticated, "refresh token expired")
		}

		if rt.Used {
			authTokenReuseTotal.Add(ctx, 1)
			slog.WarnContext(ctx, "refresh token reuse detected, revoking all tokens", "user_id", rt.UserID.String())
			if err := qtx.DeleteRefreshTokensByUserID(ctx, rt.UserID); err != nil {
				slog.ErrorContext(ctx, "failed to revoke all tokens", "error", err)
				return status.Error(codes.Internal, "failed to revoke tokens")
			}
			reuseDetected = true
			return nil
		}

		if err := qtx.MarkRefreshTokenUsed(ctx, rt.ID); err != nil {
			slog.ErrorContext(ctx, "failed to mark refresh token used", "error", err)
			return status.Error(codes.Internal, "failed to refresh token")
		}

		user, err := qtx.GetUserByID(ctx, rt.UserID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return status.Error(codes.Unauthenticated, "user no longer exists")
			}
			slog.ErrorContext(ctx, "failed to get user for refresh", "error", err)
			return status.Error(codes.Internal, "failed to get user")
		}

		accessToken, refreshToken, err = s.issueTokenPairTx(ctx, qtx, rt.UserID, user.Username)
		return err
	}); err != nil {
		return nil, err
	}

	if reuseDetected {
		return nil, status.Error(codes.Unauthenticated, "refresh token reuse detected")
	}

	return &pb.RefreshTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *Service) RevokeToken(ctx context.Context, req *pb.RevokeTokenRequest) (*pb.RevokeTokenResponse, error) {
	tokenHash := auth.HashToken(req.RefreshToken)

	if err := s.queries.DeleteRefreshTokenByHash(ctx, tokenHash); err != nil {
		slog.ErrorContext(ctx, "failed to revoke token", "error", err)
		return nil, status.Error(codes.Internal, "failed to revoke token")
	}

	return &pb.RevokeTokenResponse{}, nil
}

func (s *Service) PurgeExpiredTokens(ctx context.Context) {
	interval := s.config.Token.TokenPurgeInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.InfoContext(ctx, "Starting token purge goroutine", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "Token purge goroutine stopped")
			return
		case <-ticker.C:
			if err := s.queries.DeleteExpiredRefreshTokens(ctx); err != nil {
				slog.ErrorContext(ctx, "failed to purge expired tokens", "error", err)
			}
		}
	}
}

func (s *Service) issueTokenPairTx(ctx context.Context, qtx db.Querier, userID pgtype.UUID, username string) (string, string, error) {
	accessTokenDuration := time.Duration(s.config.Token.AccessTokenExpirationMinutes) * time.Minute
	accessToken, err := auth.GenerateJWT(userID.String(), username, s.secretKey, accessTokenDuration)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate access token", "error", err)
		return "", "", status.Error(codes.Internal, "failed to generate access token")
	}

	refreshToken := uuid.NewString()
	tokenHash := auth.HashToken(refreshToken)

	rtID, err := uuid.NewV7()
	if err != nil {
		return "", "", status.Error(codes.Internal, "failed to generate token ID")
	}
	now := time.Now()
	refreshTokenDuration := time.Duration(s.config.Token.RefreshTokenExpirationDays) * 24 * time.Hour

	if err := qtx.CreateRefreshToken(ctx, db.CreateRefreshTokenParams{
		ID:        pgtype.UUID{Bytes: rtID, Valid: true},
		UserID:    userID,
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(refreshTokenDuration), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		slog.ErrorContext(ctx, "failed to save refresh token", "error", err)
		return "", "", status.Error(codes.Internal, "failed to save refresh token")
	}

	return accessToken, refreshToken, nil
}

func (s *Service) issueTokenPair(ctx context.Context, userID pgtype.UUID, username string) (string, string, error) {
	return s.issueTokenPairTx(ctx, s.queries, userID, username)
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

		count, err := qtx.GetRoomMemberCount(ctx, roomUUID)
		if err != nil {
			return err
		}

		if int32(count) >= room.Capacity {
			roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "room_full")))
			return status.Error(codes.FailedPrecondition, "room is full")
		}

		if err := qtx.CreateRoomMember(ctx, db.CreateRoomMemberParams{
			UserID:   userUUID,
			RoomID:   roomUUID,
			JoinedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "already_member")))
				return status.Error(codes.AlreadyExists, "already a member of the room")
			}
			return err
		}
		return nil
	}); err != nil {
		st, ok := status.FromError(err)
		if ok {
			switch st.Code() {
			case codes.NotFound:
				roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "not_found")))
			case codes.AlreadyExists:
			case codes.FailedPrecondition:
			default:
				roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "internal")))
			}
			return nil, err
		}
		slog.ErrorContext(ctx, "failed to join room", "error", err)
		roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "internal")))
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
		if _, err := qtx.SoftDeleteRoom(ctx, db.SoftDeleteRoomParams{
			ID:        roomUUID,
			ManagerID: room.ManagerID,
			DeletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); err != nil {
			slog.ErrorContext(ctx, "failed to soft delete empty room", "error", err)
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

		if err := qtx.DeleteRefreshTokensByUserID(ctx, userUUID); err != nil {
			slog.ErrorContext(ctx, "failed to delete refresh tokens", "error", err)
			return status.Error(codes.Internal, "failed to revoke tokens")
		}

		if _, err := qtx.SoftDeleteUser(ctx, db.SoftDeleteUserParams{
			ID:        userUUID,
			DeletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return status.Error(codes.NotFound, "user not found")
			}
			slog.ErrorContext(ctx, "failed to soft delete user", "error", err)
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

		if _, err := qtx.SoftDeleteRoom(ctx, db.SoftDeleteRoomParams{
			ID:        roomUUID,
			ManagerID: requesterUUID,
			DeletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); err != nil {
			slog.ErrorContext(ctx, "failed to soft delete room", "error", err)
			return status.Error(codes.Internal, "failed to delete room")
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return &pb.DeleteRoomResponse{}, nil
}
