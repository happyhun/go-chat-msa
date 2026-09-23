package chat

import (
	"context"
	"log/slog"
	"time"

	pb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/shared/config"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	pb.UnsafeChatServiceServer
	config config.ChatConfig
	repo   Repository
}

func NewService(repo Repository, config config.ChatConfig) *Service {
	return &Service{
		config: config,
		repo:   repo,
	}
}

func (s *Service) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	if req.RoomId == "" {
		return nil, status.Error(codes.InvalidArgument, "room_id is required")
	}

	limit := int64(req.Limit)
	if limit <= 0 {
		limit = s.config.History.DefaultLimit
	} else if limit > s.config.History.MaxLimit {
		limit = s.config.History.MaxLimit
	}

	var joinedAt time.Time
	if req.JoinedAt != nil {
		if err := req.JoinedAt.CheckValid(); err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid joined_at")
		}
		joinedAt = req.JoinedAt.AsTime()
	}

	messages, err := s.repo.GetHistory(ctx, req.RoomId, limit+1, joinedAt)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get history", "error", err)
		return nil, status.Error(codes.Unavailable, "message history unavailable")
	}

	messages, hasMore := trimToLimit(messages, limit)
	pbMessages := messagesToProto(messages)

	chatHistoryFetchedMessages.Record(ctx, float64(len(pbMessages)))
	return &pb.ListMessagesResponse{
		Messages: pbMessages,
		HasMore:  hasMore,
	}, nil
}

func (s *Service) SyncMessages(ctx context.Context, req *pb.SyncMessagesRequest) (*pb.SyncMessagesResponse, error) {
	if req.RoomId == "" {
		return nil, status.Error(codes.InvalidArgument, "room_id is required")
	}
	afterID := req.AfterMessageId
	if afterID != "" {
		id, err := uuid.Parse(afterID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "after_message_id must be a uuid")
		}
		afterID = id.String()
	}

	limit := int64(req.Limit)
	if limit <= 0 {
		limit = s.config.Sync.DefaultLimit
	} else if limit > s.config.Sync.MaxLimit {
		limit = s.config.Sync.MaxLimit
	}

	var joinedAt time.Time
	if req.JoinedAt != nil {
		if err := req.JoinedAt.CheckValid(); err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid joined_at")
		}
		joinedAt = req.JoinedAt.AsTime()
	}

	messages, err := s.repo.SyncMessages(ctx, req.RoomId, afterID, limit+1, joinedAt)
	if err != nil {
		slog.ErrorContext(ctx, "failed to sync messages", "error", err)
		return nil, status.Error(codes.Unavailable, "message sync unavailable")
	}

	messages, hasMore := trimToLimit(messages, limit)
	pbMessages := messagesToProto(messages)

	chatHistoryFetchedMessages.Record(ctx, float64(len(pbMessages)))
	return &pb.SyncMessagesResponse{
		Messages: pbMessages,
		HasMore:  hasMore,
	}, nil
}

func trimToLimit(messages []*Message, limit int64) ([]*Message, bool) {
	if int64(len(messages)) <= limit {
		return messages, false
	}
	return messages[:limit], true
}

func messagesToProto(messages []*Message) []*pb.Message {
	pbMessages := make([]*pb.Message, 0, len(messages))
	for _, m := range messages {
		pbMessages = append(pbMessages, &pb.Message{
			Id:          m.ID,
			RoomId:      m.RoomID,
			SenderId:    m.SenderID,
			Content:     m.Content,
			ClientMsgId: m.ClientMsgID,
			Type:        m.Type,
			Timestamp:   timestamppb.New(m.CreatedAt),
		})
	}
	return pbMessages
}
