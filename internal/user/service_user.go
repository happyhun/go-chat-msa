package user

import (
	"context"
	"errors"
	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/user/db"
	"go-chat-msa/internal/user/hasher"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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
