package user

import (
	"math"
	"testing"

	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/user/db"
	dbmocks "go-chat-msa/internal/user/db/mocks"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRoomResponsesRejectOutOfRangeCounts(t *testing.T) {
	t.Parallel()
	for _, count := range []int64{-1, math.MaxInt32 + 1, math.MaxInt64} {
		t.Run("joined", func(t *testing.T) {
			queries := dbmocks.NewMockQuerier(t)
			queries.EXPECT().ListJoinedRooms(mock.Anything, mock.Anything).
				Return([]db.ListJoinedRoomsRow{{MemberCount: count}}, nil)
			resp, err := createTestService(t, queries).ListJoinedRooms(t.Context(), &pb.ListJoinedRoomsRequest{UserId: uuid.NewString()})
			assert.Nil(t, resp)
			assert.Equal(t, codes.Internal, status.Code(err))
		})
		for _, row := range []db.SearchRoomsRow{{MemberCount: count}, {TotalCount: count}} {
			t.Run("search", func(t *testing.T) {
				queries := dbmocks.NewMockQuerier(t)
				queries.EXPECT().SearchRooms(mock.Anything, mock.Anything).Return([]db.SearchRoomsRow{row}, nil)
				resp, err := createTestService(t, queries).SearchRooms(t.Context(), &pb.SearchRoomsRequest{})
				assert.Nil(t, resp)
				assert.Equal(t, codes.Internal, status.Code(err))
			})
		}
	}
}

func TestRoomResponseCountBoundaries(t *testing.T) {
	t.Parallel()
	for _, count := range []int64{0, math.MaxInt32} {
		joined, err := userRoomsFromRows([]db.ListJoinedRoomsRow{{MemberCount: count}})
		require.NoError(t, err)
		assert.EqualValues(t, count, joined[0].Room.MemberCount)
		rooms, err := roomsFromSearchRows([]db.SearchRoomsRow{{MemberCount: count}})
		require.NoError(t, err)
		assert.EqualValues(t, count, rooms[0].MemberCount)
	}
}

func TestRoomCapacityDoesNotWrapMemberCount(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"join", "update"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			queries := dbmocks.NewMockQuerier(t)
			manager := uuid.New()
			room := uuid.NewString()
			queries.EXPECT().GetRoomForUpdate(mock.Anything, mock.Anything).
				Return(db.GetRoomForUpdateRow{Capacity: 10, ManagerID: pgtype.UUID{Bytes: manager, Valid: true}}, nil)
			queries.EXPECT().GetRoomMemberCount(mock.Anything, mock.Anything).Return(int64(math.MaxInt32)+1, nil)
			service := createTestService(t, queries)
			var err error
			if operation == "join" {
				queries.EXPECT().ExistsRoomMember(mock.Anything, mock.Anything).Return(false, nil)
				_, err = service.JoinRoom(t.Context(), &pb.JoinRoomRequest{RoomId: room, UserId: manager.String()})
			} else {
				_, err = service.UpdateRoom(t.Context(), &pb.UpdateRoomRequest{Id: room, RequesterId: manager.String(), Name: "room", Capacity: 10})
			}
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		})
	}
}
