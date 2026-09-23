package user

import (
	"context"
	"errors"
	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/user/db"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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
		return s.joinRoomTx(ctx, qtx, roomUUID, userUUID)
	}); err != nil {
		st, ok := status.FromError(err)
		if ok {
			if st.Code() != codes.FailedPrecondition {
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

func (s *Service) joinRoomTx(ctx context.Context, qtx db.Querier, roomUUID, userUUID pgtype.UUID) error {
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

	if count >= int64(room.Capacity) {
		roomJoinTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
		return status.Error(codes.FailedPrecondition, "room is full")
	}

	return qtx.CreateRoomMember(ctx, db.CreateRoomMemberParams{
		UserID:   userUUID,
		RoomID:   roomUUID,
		JoinedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
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

	if !room.ManagerID.Valid || room.ManagerID.Bytes != userUUID.Bytes {
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
