package user

import (
	"context"
	"errors"
	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/user/db"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

	rooms, err := userRoomsFromRows(rows)
	if err != nil {
		return nil, err
	}
	return &pb.ListJoinedRoomsResponse{Rooms: rooms}, nil
}

func (s *Service) SearchRooms(ctx context.Context, req *pb.SearchRoomsRequest) (*pb.SearchRoomsResponse, error) {
	if req.Offset < 0 {
		return nil, status.Error(codes.InvalidArgument, "offset must not be negative")
	}
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
		totalCount, err = countToProto(rows[0].TotalCount)
		if err != nil {
			return nil, err
		}
	}

	rooms, err := roomsFromSearchRows(rows)
	if err != nil {
		return nil, err
	}
	return &pb.SearchRoomsResponse{Rooms: rooms, TotalCount: totalCount}, nil
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
		if count > int64(req.Capacity) {
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
