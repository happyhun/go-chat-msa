package websocket

import (
	"context"
	"testing"
	"time"

	chatpb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/websocket/hub"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func newTestAdapter(client *mocks.MockChatServiceClient) *chatStoreAdapter {
	return newChatStoreAdapter(client, time.Second)
}

func newTestMsgs() []*hub.Message {
	return []*hub.Message{{
		ID:             "msg-1",
		RoomID:         "room-1",
		SenderID:       "user-1",
		Content:        "hello",
		Type:           "chat",
		ClientMsgID:    "client-msg-1",
		SequenceNumber: 1,
	}}
}

func TestChatStoreAdapter_SaveMany(t *testing.T) {
	t.Parallel()

	t.Run("Success: batch request is mapped and saved", func(t *testing.T) {
		t.Parallel()

		mockClient := mocks.NewMockChatServiceClient(t)
		mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.MatchedBy(func(req *chatpb.BatchCreateMessagesRequest) bool {
			if len(req.Requests) != 1 {
				return false
			}
			got := req.Requests[0]
			return got.RoomId == "room-1" &&
				got.SenderId == "user-1" &&
				got.Content == "hello" &&
				got.Type == "chat" &&
				got.ClientMsgId == "client-msg-1" &&
				got.SequenceNumber == 1 &&
				got.MessageId == "msg-1"
		})).Return(&emptypb.Empty{}, nil)

		a := newTestAdapter(mockClient)
		assert.NoError(t, a.SaveMany(context.Background(), newTestMsgs()))
	})

	t.Run("Failure: gRPC error is returned to manager retry path", func(t *testing.T) {
		t.Parallel()

		wantErr := status.Error(codes.Unavailable, "chat-service down")
		mockClient := mocks.NewMockChatServiceClient(t)
		mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
			Return(nil, wantErr)

		a := newTestAdapter(mockClient)
		err := a.SaveMany(context.Background(), newTestMsgs())
		assert.ErrorIs(t, err, wantErr)
	})
}

func TestChatStoreAdapter_GetLastSequenceNumber(t *testing.T) {
	t.Parallel()

	mockClient := mocks.NewMockChatServiceClient(t)
	mockClient.EXPECT().GetLastSequenceNumber(mock.Anything, &chatpb.GetLastSequenceNumberRequest{
		RoomId: "room-1",
	}).Return(&chatpb.GetLastSequenceNumberResponse{SequenceNumber: 42}, nil)

	a := newTestAdapter(mockClient)
	seq, err := a.GetLastSequenceNumber(context.Background(), "room-1")
	assert.NoError(t, err)
	assert.Equal(t, int64(42), seq)
}
