package user

import (
	"math"

	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/user/db"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func userRoomsFromRows(rows []db.ListJoinedRoomsRow) ([]*pb.UserRoom, error) {
	rooms := make([]*pb.UserRoom, len(rows))
	for i, r := range rows {
		count, err := countToProto(r.MemberCount)
		if err != nil {
			return nil, err
		}
		rooms[i] = &pb.UserRoom{
			Room: &pb.Room{
				Id:          r.ID.String(),
				Name:        r.Name,
				ManagerId:   r.ManagerID.String(),
				Capacity:    r.Capacity,
				MemberCount: count,
			},
			JoinedAt: timestamppb.New(r.JoinedAt.Time),
		}
	}
	return rooms, nil
}

func roomMembersFromRows(rows []db.ListRoomMembersRow) []*pb.RoomMember {
	members := make([]*pb.RoomMember, len(rows))
	for i, r := range rows {
		members[i] = &pb.RoomMember{
			UserId:   r.ID.String(),
			Username: r.Username,
			JoinedAt: timestamppb.New(r.JoinedAt.Time),
		}
	}
	return members
}

func roomsFromSearchRows(rows []db.SearchRoomsRow) ([]*pb.Room, error) {
	rooms := make([]*pb.Room, len(rows))
	for i, r := range rows {
		count, err := countToProto(r.MemberCount)
		if err != nil {
			return nil, err
		}
		rooms[i] = &pb.Room{
			Id:          r.ID.String(),
			Name:        r.Name,
			ManagerId:   r.ManagerID.String(),
			Capacity:    r.Capacity,
			MemberCount: count,
		}
	}
	return rooms, nil
}

func countToProto(count int64) (int32, error) {
	if count < 0 || count > math.MaxInt32 {
		return 0, status.Error(codes.Internal, "count exceeds response range")
	}
	return int32(count), nil
}
