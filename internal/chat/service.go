package chat

import (
	"context"
	"log/slog"
	"time"
	"unicode/utf8"

	pb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/shared/config"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
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

func (s *Service) BatchCreateMessages(ctx context.Context, req *pb.BatchCreateMessagesRequest) (*emptypb.Empty, error) {
	if len(req.Requests) == 0 {
		return &emptypb.Empty{}, nil
	}

	msgs := make([]*Message, 0, len(req.Requests))
	for _, r := range req.Requests {
		if r.RoomId == "" || r.SenderId == "" || r.Content == "" {
			return nil, status.Error(codes.InvalidArgument, "room_id, sender_id, and content are required")
		}
		msg, err := s.messageFromRequest(ctx, r)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}

	if err := s.repo.SaveMany(ctx, msgs); err != nil {
		slog.ErrorContext(ctx, "failed to batch save messages", "count", len(msgs), "error", err)
		chatMessagesSavedTotal.Add(ctx, int64(len(msgs)), metric.WithAttributes(attribute.String("status", "error")))
		return nil, status.Errorf(codes.Internal, "failed to batch save messages: %v", err)
	}
	chatMessagesSavedTotal.Add(ctx, int64(len(msgs)), metric.WithAttributes(attribute.String("status", "ok")))

	return &emptypb.Empty{}, nil
}

func (s *Service) messageFromRequest(ctx context.Context, req *pb.CreateMessageRequest) (*Message, error) {
	if utf8.RuneCountInString(req.Content) > s.config.Message.MaxLength {
		return nil, status.Errorf(codes.InvalidArgument, "content exceeds max length of %d characters", s.config.Message.MaxLength)
	}

	var msgID uuid.UUID
	var err error
	if req.MessageId != "" {
		msgID, err = uuid.Parse(req.MessageId)
	} else {
		msgID, err = uuid.NewV7()
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to handle message uuid", "error", err)
		return nil, status.Error(codes.Internal, "failed to handle message uuid")
	}

	createdAt := time.Now()
	if msgID.Version() == 7 {
		createdAt = time.Unix(msgID.Time().UnixTime())
	}

	return &Message{
		ID:             msgID.String(),
		RoomID:         req.RoomId,
		SenderID:       req.SenderId,
		Content:        req.Content,
		Type:           req.Type,
		ClientMsgID:    req.ClientMsgId,
		SequenceNumber: req.SequenceNumber,
		CreatedAt:      createdAt,
	}, nil
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
		joinedAt = req.JoinedAt.AsTime()
	}

	messages, err := s.repo.GetHistory(ctx, req.RoomId, limit, joinedAt)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get history", "error", err)
		return nil, status.Errorf(codes.Internal, "failed to get history: %v", err)
	}

	pbMessages := make([]*pb.Message, 0, len(messages))
	for _, m := range messages {
		pbMessages = append(pbMessages, &pb.Message{
			Id:             m.ID,
			RoomId:         m.RoomID,
			SenderId:       m.SenderID,
			Content:        m.Content,
			Type:           m.Type,
			Timestamp:      timestamppb.New(m.CreatedAt),
			SequenceNumber: m.SequenceNumber,
		})
	}

	chatHistoryFetchedMessages.Record(ctx, float64(len(pbMessages)))
	return &pb.ListMessagesResponse{
		Messages: pbMessages,
	}, nil
}

func (s *Service) SyncMessages(ctx context.Context, req *pb.SyncMessagesRequest) (*pb.SyncMessagesResponse, error) {
	if req.RoomId == "" {
		return nil, status.Error(codes.InvalidArgument, "room_id is required")
	}

	limit := int64(req.Limit)
	if limit <= 0 {
		limit = s.config.Sync.DefaultLimit
	} else if limit > s.config.Sync.MaxLimit {
		limit = s.config.Sync.MaxLimit
	}

	var joinedAt time.Time
	if req.JoinedAt != nil {
		joinedAt = req.JoinedAt.AsTime()
	}

	messages, err := s.repo.SyncMessages(ctx, req.RoomId, req.LastSequenceNumber, limit, joinedAt)
	if err != nil {
		slog.ErrorContext(ctx, "failed to sync messages", "error", err)
		return nil, status.Errorf(codes.Internal, "failed to sync messages: %v", err)
	}

	pbMessages := make([]*pb.Message, 0, len(messages))
	for _, m := range messages {
		pbMessages = append(pbMessages, &pb.Message{
			Id:             m.ID,
			RoomId:         m.RoomID,
			SenderId:       m.SenderID,
			Content:        m.Content,
			Type:           m.Type,
			Timestamp:      timestamppb.New(m.CreatedAt),
			SequenceNumber: m.SequenceNumber,
		})
	}

	chatHistoryFetchedMessages.Record(ctx, float64(len(pbMessages)))
	return &pb.SyncMessagesResponse{
		Messages: pbMessages,
	}, nil
}

func (s *Service) GetLastSequenceNumber(ctx context.Context, req *pb.GetLastSequenceNumberRequest) (*pb.GetLastSequenceNumberResponse, error) {
	if req.RoomId == "" {
		return nil, status.Error(codes.InvalidArgument, "room_id is required")
	}

	seq, err := s.repo.GetLastSequenceNumber(ctx, req.RoomId)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get last sequence number", "error", err)
		return nil, status.Errorf(codes.Internal, "failed to get last sequence number: %v", err)
	}

	return &pb.GetLastSequenceNumberResponse{
		SequenceNumber: seq,
	}, nil
}
