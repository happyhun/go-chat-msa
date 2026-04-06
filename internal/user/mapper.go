package user

import (
	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/user/db"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func userRoomsFromRows(rows []db.ListJoinedRoomsRow) []*pb.UserRoom {
	rooms := make([]*pb.UserRoom, len(rows))
	for i, r := range rows {
		rooms[i] = &pb.UserRoom{
			Room: &pb.Room{
				Id:          r.ID.String(),
				Name:        r.Name,
				ManagerId:   r.ManagerID.String(),
				Capacity:    r.Capacity,
				MemberCount: int32(r.MemberCount),
			},
			JoinedAt: timestamppb.New(r.JoinedAt.Time),
		}
	}
	return rooms
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

func roomsFromSearchRows(rows []db.SearchRoomsRow) []*pb.Room {
	rooms := make([]*pb.Room, len(rows))
	for i, r := range rows {
		rooms[i] = &pb.Room{
			Id:          r.ID.String(),
			Name:        r.Name,
			ManagerId:   r.ManagerID.String(),
			Capacity:    r.Capacity,
			MemberCount: int32(r.MemberCount),
		}
	}
	return rooms
}
