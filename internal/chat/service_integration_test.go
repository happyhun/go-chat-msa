//go:build integration

package chat_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	pb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/chat"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/database"

	"github.com/golang-migrate/migrate/v4"
	mongodb_migrate "github.com/golang-migrate/migrate/v4/database/mongodb"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type chatIntegrationConfig struct {
	ChatService config.ChatConfig `mapstructure:"CHAT_SERVICE" validate:"required"`
}

type ChatSuite struct {
	suite.Suite
	container   *mongodb.MongoDBContainer
	mongoClient *mongo.Client
	client      *chat.Service
	repo        chat.Repository
}

func (s *ChatSuite) SetupSuite() {
	ctx := context.Background()

	mongoContainer, err := mongodb.Run(ctx, "mongo:6")
	s.Require().NoError(err)
	s.container = mongoContainer

	uri, err := mongoContainer.ConnectionString(ctx)
	s.Require().NoError(err)

	dbClient, err := database.NewMongo(uri)
	s.Require().NoError(err)

	s.mongoClient = dbClient

	cfgPath, err := filepath.Abs("../../configs")
	s.Require().NoError(err)

	cfg, err := config.Load[chatIntegrationConfig](cfgPath, "base", "")
	s.Require().NoError(err)

	s.runMigrations()

	col := dbClient.Database("chat_service").Collection("messages")
	s.repo = chat.NewRepository(col)
	s.client = chat.NewService(s.repo, cfg.ChatService)
}

func (s *ChatSuite) TearDownSuite() {
	if s.mongoClient != nil {
		_ = s.mongoClient.Disconnect(context.Background())
	}
	if s.container != nil {
		s.container.Terminate(context.Background())
	}
}

func (s *ChatSuite) SetupTest() {
	db := s.mongoClient.Database("chat_service")
	err := db.Collection("messages").Drop(s.T().Context())
	s.Require().NoError(err)
	_ = db.Collection("schema_migrations").Drop(s.T().Context())

	s.runMigrations()
}

func (s *ChatSuite) runMigrations() {
	migrationsDir, err := filepath.Abs("../../db/migrations/mongo")
	s.Require().NoError(err)

	driver, err := mongodb_migrate.WithInstance(s.mongoClient, &mongodb_migrate.Config{
		DatabaseName: "chat_service",
	})
	s.Require().NoError(err)

	m, err := migrate.NewWithDatabaseInstance(
		"file://"+migrationsDir,
		"mongodb",
		driver,
	)
	s.Require().NoError(err)

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		s.Require().NoError(err)
	}
}

func (s *ChatSuite) TestBatchCreateMessages_Success() {
	req := &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId:      "room_integration_1",
			SenderId:    "user_1",
			Content:     "Hello Integration",
			ClientMsgId: "msg_int_1",
		}},
	}

	_, err := s.client.BatchCreateMessages(s.T().Context(), req)

	s.Require().NoError(err)
}

func (s *ChatSuite) TestBatchCreateMessages_DuplicateIdempotent() {
	req := &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId:         "room_integration_1",
			SenderId:       "user_1",
			Content:        "Duplicate Content",
			ClientMsgId:    "msg_int_dup",
			SequenceNumber: 1,
		}},
	}

	_, err := s.client.BatchCreateMessages(s.T().Context(), req)
	s.Require().NoError(err)

	_, err = s.client.BatchCreateMessages(s.T().Context(), req)
	s.Require().NoError(err)
}

func (s *ChatSuite) TestListMessages_DescSort() {
	roomID := "room_history_sort"
	_ = sendHelperWithSeq(s, roomID, "u1", "First", "m1", 1)
	_ = sendHelperWithSeq(s, roomID, "u2", "Second", "m2", 2)

	req := &pb.ListMessagesRequest{
		RoomId: roomID,
		Limit:  10,
	}

	res, err := s.client.ListMessages(s.T().Context(), req)

	s.Require().NoError(err)
	s.Require().Len(res.Messages, 2)
	s.Equal(int64(2), res.Messages[0].SequenceNumber)
	s.Equal("Second", res.Messages[0].Content)
	s.Equal(int64(1), res.Messages[1].SequenceNumber)
	s.Equal("First", res.Messages[1].Content)
}

func (s *ChatSuite) TestListMessages_Pagination() {
	roomID := "room_history_paging"
	for i := 1; i <= 5; i++ {
		_ = sendHelperWithSeq(s, roomID, "u1", "Msg "+fmt.Sprint(i), "m"+fmt.Sprint(i), int64(i))
	}

	req := &pb.ListMessagesRequest{
		RoomId: roomID,
		Limit:  3,
	}
	res, err := s.client.ListMessages(s.T().Context(), req)

	s.Require().NoError(err)
	s.Len(res.Messages, 3)
	s.Equal("Msg 5", res.Messages[0].Content)
	s.Equal("Msg 4", res.Messages[1].Content)
	s.Equal("Msg 3", res.Messages[2].Content)
}

func (s *ChatSuite) TestListMessages_JoinedAtFiltering() {
	roomID := "room_history_joined"
	sender := "u1"

	u1, _ := uuid.NewV7()
	_, _ = s.client.BatchCreateMessages(s.T().Context(), &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId: roomID, SenderId: sender, Content: "Old Msg", ClientMsgId: "m1", SequenceNumber: 1, MessageId: u1.String(),
		}},
	})

	time.Sleep(10 * time.Millisecond)

	u_ref, _ := uuid.NewV7()
	joinedAt := time.Unix(u_ref.Time().UnixTime())

	time.Sleep(10 * time.Millisecond)

	u2, _ := uuid.NewV7()
	_, _ = s.client.BatchCreateMessages(s.T().Context(), &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId: roomID, SenderId: sender, Content: "New Msg", ClientMsgId: "m2", SequenceNumber: 2, MessageId: u2.String(),
		}},
	})

	resAll, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{
		RoomId: roomID,
		Limit:  10,
	})
	s.Require().NoError(err)
	s.Len(resAll.Messages, 2)

	resFiltered, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{
		RoomId:   roomID,
		Limit:    10,
		JoinedAt: timestamppb.New(joinedAt),
	})
	s.Require().NoError(err)
	s.Require().Len(resFiltered.Messages, 1)
	s.Equal("New Msg", resFiltered.Messages[0].Content)
}

func (s *ChatSuite) TestSequence_UniqueConstraint_Idempotent() {
	roomID := "room_unique_seq"

	err := sendHelperWithSeq(s, roomID, "u1", "Msg 1", "m1", 1)
	s.Require().NoError(err)

	err = sendHelperWithSeq(s, roomID, "u1", "Msg 2", "m2", 1)
	s.Require().NoError(err)

	res, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: roomID, Limit: 10})
	s.Require().NoError(err)
	s.Len(res.Messages, 1)
}

func (s *ChatSuite) TestGetLastSequenceNumber() {
	roomID := "room_latest_seq"

	for i := 1; i <= 5; i++ {
		err := sendHelperWithSeq(s, roomID, "u1", "Msg", "m"+fmt.Sprint(i), int64(i))
		s.Require().NoError(err)
	}

	res, err := s.client.GetLastSequenceNumber(s.T().Context(), &pb.GetLastSequenceNumberRequest{RoomId: roomID})
	s.Require().NoError(err)
	s.Equal(int64(5), res.SequenceNumber)
}

func (s *ChatSuite) TestSyncMessages() {
	roomID := "room_sync"

	for i := 1; i <= 10; i++ {
		_ = sendHelperWithSeq(s, roomID, "u1", "Content "+fmt.Sprint(i), "id"+fmt.Sprint(i), int64(i))
	}

	req := &pb.SyncMessagesRequest{
		RoomId:             roomID,
		LastSequenceNumber: 5,
		Limit:              3,
	}

	res, err := s.client.SyncMessages(s.T().Context(), req)

	s.Require().NoError(err)
	s.Require().Len(res.Messages, 3)
	s.Equal(int64(6), res.Messages[0].SequenceNumber)
	s.Equal(int64(7), res.Messages[1].SequenceNumber)
	s.Equal(int64(8), res.Messages[2].SequenceNumber)
}

func (s *ChatSuite) TestSyncMessages_JoinedAtFiltering() {
	roomID := uuid.New().String()
	sender := "u1"

	u1, _ := uuid.NewV7()
	_, _ = s.client.BatchCreateMessages(s.T().Context(), &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId: roomID, SenderId: sender, Content: "Msg 1", ClientMsgId: "m1", SequenceNumber: 1, MessageId: u1.String(),
		}},
	})

	time.Sleep(10 * time.Millisecond)

	u_ref, _ := uuid.NewV7()
	joinedAt := time.Unix(u_ref.Time().UnixTime())

	time.Sleep(10 * time.Millisecond)

	u2, _ := uuid.NewV7()
	_, _ = s.client.BatchCreateMessages(s.T().Context(), &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId: roomID, SenderId: sender, Content: "Msg 2", ClientMsgId: "m2", SequenceNumber: 2, MessageId: u2.String(),
		}},
	})
	u3, _ := uuid.NewV7()
	_, _ = s.client.BatchCreateMessages(s.T().Context(), &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId: roomID, SenderId: sender, Content: "Msg 3", ClientMsgId: "m3", SequenceNumber: 3, MessageId: u3.String(),
		}},
	})

	res, err := s.client.SyncMessages(s.T().Context(), &pb.SyncMessagesRequest{
		RoomId:             roomID,
		LastSequenceNumber: 0,
		Limit:              10,
		JoinedAt:           timestamppb.New(joinedAt),
	})

	s.Require().NoError(err)
	s.Require().Len(res.Messages, 2)
	s.Equal("Msg 2", res.Messages[0].Content)
	s.Equal("Msg 3", res.Messages[1].Content)
}

func (s *ChatSuite) TestBatchCreateMessages_MissingRequiredFields() {
	tests := []struct {
		name    string
		roomID  string
		sender  string
		content string
	}{
		{"room_id 누락", "", "user_1", "Hello"},
		{"sender_id 누락", "room_1", "", "Hello"},
		{"content 누락", "room_1", "user_1", ""},
	}

	for _, tt := range tests {
		s.Run("Failure: "+tt.name+" (InvalidArgument)", func() {
			req := &pb.BatchCreateMessagesRequest{
				Requests: []*pb.CreateMessageRequest{{
					RoomId:      tt.roomID,
					SenderId:    tt.sender,
					Content:     tt.content,
					ClientMsgId: "msg_fail",
				}},
			}
			_, err := s.client.BatchCreateMessages(s.T().Context(), req)
			s.Require().Error(err)
			s.Contains(err.Error(), "required")
		})
	}
}

func (s *ChatSuite) TestBatchCreateMessages_EmptyRequests() {
	req := &pb.BatchCreateMessagesRequest{Requests: nil}

	_, err := s.client.BatchCreateMessages(s.T().Context(), req)

	s.NoError(err)
}

func (s *ChatSuite) TestListMessages_EmptyRoomID() {
	req := &pb.ListMessagesRequest{
		RoomId: "",
		Limit:  10,
	}

	_, err := s.client.ListMessages(s.T().Context(), req)

	s.Require().Error(err)
	s.Contains(err.Error(), "room_id is required")
}

func (s *ChatSuite) TestListMessages_NoMessages() {
	req := &pb.ListMessagesRequest{
		RoomId: "room_nonexistent",
		Limit:  10,
	}

	res, err := s.client.ListMessages(s.T().Context(), req)

	s.Require().NoError(err)
	s.Empty(res.Messages)
}

func (s *ChatSuite) TestSyncMessages_EmptyRoomID() {
	req := &pb.SyncMessagesRequest{
		RoomId:             "",
		LastSequenceNumber: 0,
		Limit:              10,
	}

	_, err := s.client.SyncMessages(s.T().Context(), req)

	s.Require().Error(err)
	s.Contains(err.Error(), "room_id is required")
}

func sendHelperWithSeq(s *ChatSuite, roomID, senderID, content, clientMsgID string, seq int64) error {
	_, err := s.client.BatchCreateMessages(s.T().Context(), &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId:         roomID,
			SenderId:       senderID,
			Content:        content,
			ClientMsgId:    clientMsgID,
			SequenceNumber: seq,
		}},
	})
	return err
}

func sendHelper(s *ChatSuite, roomID, senderID, content, msgID string) error {
	_, err := s.client.BatchCreateMessages(s.T().Context(), &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId:      roomID,
			SenderId:    senderID,
			Content:     content,
			ClientMsgId: msgID,
		}},
	})
	return err
}

func (s *ChatSuite) TestBatchCreateMessages_TimestampSync() {
	req := &pb.BatchCreateMessagesRequest{
		Requests: []*pb.CreateMessageRequest{{
			RoomId:      "ts-room",
			SenderId:    "ts-user",
			Content:     "Timestamp Sync Test",
			ClientMsgId: "ts-msg-id",
		}},
	}

	before := time.Now()
	_, err := s.client.BatchCreateMessages(s.T().Context(), req)
	after := time.Now()

	s.Require().NoError(err)

	dbMsg, err := s.repo.GetHistory(s.T().Context(), "ts-room", 1, time.Time{})
	s.Require().NoError(err)
	s.Require().Len(dbMsg, 1)

	msgIDParsed, err := uuid.Parse(dbMsg[0].ID)
	s.Require().NoError(err)

	createdAt := dbMsg[0].CreatedAt.In(time.UTC)

	sec, nsec := msgIDParsed.Time().UnixTime()
	uuidTime := time.Unix(sec, nsec).In(time.UTC)

	s.Equal(uuidTime.UnixMilli(), createdAt.UnixMilli(), "UUID timestamp and MongoDB CreatedAt should match in milliseconds")

	s.True(createdAt.After(before.Add(-1 * time.Second)))
	s.True(createdAt.Before(after.Add(1 * time.Second)))
}

func TestChatSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	suite.Run(t, new(ChatSuite))
}
