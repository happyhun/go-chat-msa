package websocket

import (
	"context"
	"time"

	chatpb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/websocket/hub"
)

type chatStoreAdapter struct {
	client     chatpb.ChatServiceClient
	rpcTimeout time.Duration
}

func newChatStoreAdapter(client chatpb.ChatServiceClient, rpcTimeout time.Duration) *chatStoreAdapter {
	return &chatStoreAdapter{
		client:     client,
		rpcTimeout: rpcTimeout,
	}
}

func (a *chatStoreAdapter) GetLastSequenceNumber(ctx context.Context, roomID string) (int64, error) {
	resp, err := a.client.GetLastSequenceNumber(ctx, &chatpb.GetLastSequenceNumberRequest{
		RoomId: roomID,
	})
	if err != nil {
		return 0, err
	}
	return resp.SequenceNumber, nil
}

func (a *chatStoreAdapter) SaveMany(ctx context.Context, msgs []*hub.Message) error {
	ctx, cancel := context.WithTimeout(ctx, a.rpcTimeout)
	defer cancel()

	reqs := make([]*chatpb.CreateMessageRequest, len(msgs))
	for i, msg := range msgs {
		reqs[i] = &chatpb.CreateMessageRequest{
			RoomId:         msg.RoomID,
			SenderId:       msg.SenderID,
			Content:        msg.Content,
			ClientMsgId:    msg.ClientMsgID,
			Type:           msg.Type,
			SequenceNumber: msg.SequenceNumber,
			MessageId:      msg.ID,
		}
	}
	_, err := a.client.BatchCreateMessages(ctx, &chatpb.BatchCreateMessagesRequest{Requests: reqs})
	return err
}
