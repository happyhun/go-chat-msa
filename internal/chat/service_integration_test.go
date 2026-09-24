//go:build integration

package chat_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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
	"github.com/testcontainers/testcontainers-go"
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

	mongoContainer, err := mongodb.Run(ctx, "mongo:7.0", testcontainers.WithAlwaysPull())
	s.Require().NoError(err)
	s.container = mongoContainer

	uri, err := mongoContainer.ConnectionString(ctx)
	s.Require().NoError(err)

	dbClient, err := database.NewMongo(uri)
	s.Require().NoError(err)

	s.mongoClient = dbClient

	cfgPath, err := filepath.Abs("../../deploy/k8s/base/apps/config/app")
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
		s.Require().NoError(s.mongoClient.Disconnect(context.Background()))
	}
	if s.container != nil {
		s.container.Terminate(context.Background())
	}
}

func (s *ChatSuite) SetupTest() {
	db := s.mongoClient.Database("chat_service")
	err := db.Collection("messages").Drop(s.T().Context())
	s.Require().NoError(err)
	err = db.Collection("schema_migrations").Drop(s.T().Context())
	s.Require().NoError(err)

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

func (s *ChatSuite) TestSaveBatch_Success() {
	s.Require().NoError(sendHelper(s, "room_integration_1", "user_1", "Hello Integration", "msg_int_1"))
}

func (s *ChatSuite) TestSaveBatch_DuplicateIdempotent() {
	for range 2 {
		s.Require().NoError(sendHelper(s, "room_integration_1", "user_1", "Duplicate Content", "msg_int_dup"))
	}
	res, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: "room_integration_1", Limit: 10})
	s.Require().NoError(err)
	s.Len(res.Messages, 1)
}

func (s *ChatSuite) TestListMessages_IDDescSort() {
	roomID := "room_history_sort"
	s.Require().NoError(sendHelper(s, roomID, "u1", "First", "m1"))
	time.Sleep(5 * time.Millisecond)
	s.Require().NoError(sendHelper(s, roomID, "u2", "Second", "m2"))

	res, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: roomID, Limit: 10})

	s.Require().NoError(err)
	s.Require().Len(res.Messages, 2)
	s.Equal("Second", res.Messages[0].Content)
	s.Equal("First", res.Messages[1].Content)
	s.Greater(res.Messages[0].Id, res.Messages[1].Id, "id 역순으로 반환한다")
}

func (s *ChatSuite) TestListMessages_PaginationHasMore() {
	roomID := "room_history_paging"
	for i := 1; i <= 5; i++ {
		s.Require().NoError(sendHelper(s, roomID, "u1", "Msg "+fmt.Sprint(i), "m"+fmt.Sprint(i)))
		time.Sleep(2 * time.Millisecond)
	}

	res, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: roomID, Limit: 3})

	s.Require().NoError(err)
	s.Len(res.Messages, 3)
	s.True(res.HasMore, "남은 분량이 있으면 has_more가 참이다")
	s.Equal("Msg 5", res.Messages[0].Content)
	s.Equal("Msg 3", res.Messages[2].Content)

	all, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: roomID, Limit: 10})
	s.Require().NoError(err)
	s.False(all.HasMore)
}

func (s *ChatSuite) TestListMessages_JoinedAtFiltering() {
	roomID := "room_history_joined"
	sender := "u1"

	s.Require().NoError(sendHelper(s, roomID, sender, "Old Msg", "m1"))

	time.Sleep(10 * time.Millisecond)
	uRef, err := uuid.NewV7()
	s.Require().NoError(err)
	joinedAt := time.Unix(uRef.Time().UnixTime())
	time.Sleep(10 * time.Millisecond)

	s.Require().NoError(sendHelper(s, roomID, sender, "New Msg", "m2"))

	resAll, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: roomID, Limit: 10})
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

func (s *ChatSuite) TestSyncMessages_AfterMessageID() {
	roomID := "room_sync"

	ids := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		s.Require().NoError(sendHelper(s, roomID, "u1", "Content "+fmt.Sprint(i), "id"+fmt.Sprint(i)))
		time.Sleep(2 * time.Millisecond)
	}

	history, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: roomID, Limit: 10})
	s.Require().NoError(err)
	for i := len(history.Messages) - 1; i >= 0; i-- {
		ids = append(ids, history.Messages[i].Id)
	}
	s.Require().Len(ids, 10)

	res, err := s.client.SyncMessages(s.T().Context(), &pb.SyncMessagesRequest{
		RoomId:         roomID,
		AfterMessageId: ids[4],
		Limit:          3,
	})

	s.Require().NoError(err)
	s.Require().Len(res.Messages, 3)
	s.True(res.HasMore)
	s.Equal("Content 6", res.Messages[0].Content)
	s.Equal("Content 7", res.Messages[1].Content)
	s.Equal("Content 8", res.Messages[2].Content)
}

func (s *ChatSuite) TestSyncMessages_CursorFormats() {
	const roomID = "room_cursor"
	ids := []string{
		"01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d8d",
		"01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d8e",
		"01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d8f",
		"01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d90",
	}
	var messages []*chat.Message
	for _, id := range ids {
		messages = append(messages, &chat.Message{
			ID: id, RoomID: roomID, SenderID: "u1", ClientMsgID: id,
			Content: id, Type: "chat", CreatedAt: time.Now(),
		})
	}
	for _, err := range s.repo.SaveBatch(s.T().Context(), messages) {
		s.Require().NoError(err)
	}

	for _, cursor := range []string{
		ids[1], strings.ToUpper(ids[1]), strings.ReplaceAll(ids[1], "-", ""), "urn:uuid:" + ids[1],
	} {
		s.Run(cursor, func() {
			res, err := s.client.SyncMessages(s.T().Context(), &pb.SyncMessagesRequest{
				RoomId: roomID, AfterMessageId: cursor, Limit: 1,
			})
			s.Require().NoError(err)
			s.Require().Len(res.Messages, 1)
			s.Equal(ids[2], res.Messages[0].Id)
			s.True(res.HasMore)

			next, err := s.client.SyncMessages(s.T().Context(), &pb.SyncMessagesRequest{
				RoomId: roomID, AfterMessageId: res.Messages[0].Id, Limit: 1,
			})
			s.Require().NoError(err)
			s.Require().Len(next.Messages, 1)
			s.Equal(ids[3], next.Messages[0].Id)
			s.False(next.HasMore)
		})
	}
}

func (s *ChatSuite) TestSyncMessages_InvalidAfterMessageID() {
	_, err := s.client.SyncMessages(s.T().Context(), &pb.SyncMessagesRequest{
		RoomId:         "room_sync_invalid",
		AfterMessageId: "not-a-uuid",
		Limit:          10,
	})

	s.Require().Error(err)
	s.Contains(err.Error(), "after_message_id")
}

func (s *ChatSuite) TestSyncMessages_JoinedAtFiltering() {
	roomID := uuid.New().String()
	sender := "u1"

	s.Require().NoError(sendHelper(s, roomID, sender, "Msg 1", "m1"))

	time.Sleep(10 * time.Millisecond)
	uRef, err := uuid.NewV7()
	s.Require().NoError(err)
	joinedAt := time.Unix(uRef.Time().UnixTime())
	time.Sleep(10 * time.Millisecond)

	s.Require().NoError(sendHelper(s, roomID, sender, "Msg 2", "m2"))
	time.Sleep(2 * time.Millisecond)
	s.Require().NoError(sendHelper(s, roomID, sender, "Msg 3", "m3"))

	res, err := s.client.SyncMessages(s.T().Context(), &pb.SyncMessagesRequest{
		RoomId:   roomID,
		Limit:    10,
		JoinedAt: timestamppb.New(joinedAt),
	})

	s.Require().NoError(err)
	s.Require().Len(res.Messages, 2)
	s.Equal("Msg 2", res.Messages[0].Content)
	s.Equal("Msg 3", res.Messages[1].Content)
}

func (s *ChatSuite) TestSaveBatch_EmptyMessages() {
	s.Empty(s.repo.SaveBatch(s.T().Context(), nil))
}

func (s *ChatSuite) TestListMessages_EmptyRoomID() {
	_, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: "", Limit: 10})

	s.Require().Error(err)
	s.Contains(err.Error(), "room_id is required")
}

func (s *ChatSuite) TestListMessages_NoMessages() {
	res, err := s.client.ListMessages(s.T().Context(), &pb.ListMessagesRequest{RoomId: "room_nonexistent", Limit: 10})

	s.Require().NoError(err)
	s.Empty(res.Messages)
	s.False(res.HasMore)
}

func (s *ChatSuite) TestSyncMessages_EmptyRoomID() {
	_, err := s.client.SyncMessages(s.T().Context(), &pb.SyncMessagesRequest{RoomId: "", Limit: 10})

	s.Require().Error(err)
	s.Contains(err.Error(), "room_id is required")
}

func (s *ChatSuite) TestSaveBatch_TimestampSync() {
	before := time.Now()
	err := sendHelper(s, "ts-room", "ts-user", "Timestamp Sync Test", "ts-msg-id")
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

func sendHelper(s *ChatSuite, roomID, senderID, content, clientMsgID string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	sec, nsec := id.Time().UnixTime()
	results := s.repo.SaveBatch(s.T().Context(), []*chat.Message{{
		ID: id.String(), RoomID: roomID, SenderID: senderID, Content: content, ClientMsgID: clientMsgID, Type: "chat", CreatedAt: time.Unix(sec, nsec),
	}})
	return results[0]
}

func TestChatSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	suite.Run(t, new(ChatSuite))
}
