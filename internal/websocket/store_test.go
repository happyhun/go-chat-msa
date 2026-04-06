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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func newTestAdapter(t *testing.T, client *mocks.MockChatServiceClient) *chatStoreAdapter {
	t.Helper()
	return newChatStoreAdapter(client, 1*time.Second)
}

func newTestMsgs() []*hub.Message {
	return []*hub.Message{{
		ID:             "msg-1",
		RoomID:         "room-1",
		SenderID:       "user-1",
		Content:        "hello",
		Type:           "chat",
		SequenceNumber: 1,
	}}
}

func TestChatStoreAdapter_SaveMany(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mockBehavior func(m *mocks.MockChatServiceClient)
		wantEnqueued bool
	}{
		{
			name: "Success: 저장 성공 시 재시도 큐에 추가하지 않음",
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(&emptypb.Empty{}, nil)
			},
			wantEnqueued: false,
		},
		{
			name: "Success: 영구 실패는 재시도 없이 폐기 (InvalidArgument)",
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.InvalidArgument, "bad request"))
			},
			wantEnqueued: false,
		},
		{
			name: "Success: 영구 실패는 재시도 없이 폐기 (AlreadyExists)",
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.AlreadyExists, "duplicate"))
			},
			wantEnqueued: false,
		},
		{
			name: "Success: 일시적 에러는 재시도 큐에 추가 (Unavailable)",
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Unavailable, "service unavailable"))
			},
			wantEnqueued: true,
		},
		{
			name: "Success: 일시적 에러는 재시도 큐에 추가 (DeadlineExceeded)",
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.DeadlineExceeded, "timeout"))
			},
			wantEnqueued: true,
		},
		{
			name: "Success: 일시적 에러는 재시도 큐에 추가 (Internal)",
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Internal, "internal error"))
			},
			wantEnqueued: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockClient := mocks.NewMockChatServiceClient(t)
			tt.mockBehavior(mockClient)

			a := newTestAdapter(t, mockClient)
			a.SaveMany(context.Background(), newTestMsgs())

			if tt.wantEnqueued {
				assert.Len(t, a.retryCh, 1)
			} else {
				assert.Empty(t, a.retryCh)
			}
		})
	}
}

func TestChatStoreAdapter_SaveMany_QueueFull(t *testing.T) {
	t.Parallel()

	mockClient := mocks.NewMockChatServiceClient(t)
	mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.Unavailable, "down")).Times(3)

	a := newTestAdapter(t, mockClient)
	a.retryCh = make(chan batchRetryTask, 2)

	for range 3 {
		a.SaveMany(context.Background(), newTestMsgs())
	}

	assert.Len(t, a.retryCh, 2, "큐는 최대 크기를 초과하지 않아야 한다")
}

func TestChatStoreAdapter_ProcessBatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		task          batchRetryTask
		mockBehavior  func(m *mocks.MockChatServiceClient)
		wantRemaining int
	}{
		{
			name: "Success: 재시도 성공 시 batch에서 제거",
			task: batchRetryTask{msgs: newTestMsgs(), attempts: 1, nextRetry: time.Now().Add(-1 * time.Millisecond)},
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(&emptypb.Empty{}, nil)
			},
			wantRemaining: 0,
		},
		{
			name: "Success: AlreadyExists는 재시도 성공으로 처리 후 제거 (AlreadyExists)",
			task: batchRetryTask{msgs: newTestMsgs(), attempts: 1, nextRetry: time.Now().Add(-1 * time.Millisecond)},
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.AlreadyExists, "duplicate"))
			},
			wantRemaining: 0,
		},
		{
			name: "Success: nextRetry 미도래 시 처리하지 않고 유지",
			task: batchRetryTask{msgs: newTestMsgs(), attempts: 1, nextRetry: time.Now().Add(10 * time.Second)},
			mockBehavior: func(m *mocks.MockChatServiceClient) {
			},
			wantRemaining: 1,
		},
		{
			name: "Success: 재시도 실패 시 attempts 증가 후 유지 (Unavailable)",
			task: batchRetryTask{msgs: newTestMsgs(), attempts: 1, nextRetry: time.Now().Add(-1 * time.Millisecond)},
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Unavailable, "down"))
			},
			wantRemaining: 1,
		},
		{
			name: "Success: MaxAttempts 초과 시 폐기 (Unavailable)",
			task: batchRetryTask{msgs: newTestMsgs(), attempts: retryMaxAttempts, nextRetry: time.Now().Add(-1 * time.Millisecond)},
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.Unavailable, "down"))
			},
			wantRemaining: 0,
		},
		{
			name: "Success: Non-retryable 에러 시 즉시 폐기 (InvalidArgument)",
			task: batchRetryTask{msgs: newTestMsgs(), attempts: 1, nextRetry: time.Now().Add(-1 * time.Millisecond)},
			mockBehavior: func(m *mocks.MockChatServiceClient) {
				m.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
					Return(nil, status.Error(codes.InvalidArgument, "bad"))
			},
			wantRemaining: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockClient := mocks.NewMockChatServiceClient(t)
			tt.mockBehavior(mockClient)

			a := newTestAdapter(t, mockClient)
			remaining := a.processBatch(context.Background(), []batchRetryTask{tt.task})

			assert.Len(t, remaining, tt.wantRemaining)
		})
	}
}

func TestChatStoreAdapter_ProcessBatch_AttemptsIncrement(t *testing.T) {
	t.Parallel()

	mockClient := mocks.NewMockChatServiceClient(t)
	mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.Unavailable, "down"))

	a := newTestAdapter(t, mockClient)
	task := batchRetryTask{msgs: newTestMsgs(), attempts: 1, nextRetry: time.Now().Add(-1 * time.Millisecond)}

	beforeProcess := time.Now()
	remaining := a.processBatch(context.Background(), []batchRetryTask{task})

	assert.Len(t, remaining, 1)
	assert.Equal(t, 2, remaining[0].attempts, "attempts는 1 증가해야 한다")
	assert.True(t, remaining[0].nextRetry.After(beforeProcess), "nextRetry는 미래여야 한다")
}

func TestChatStoreAdapter_DrainBatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		batch     []batchRetryTask
		inChannel int
		mockTimes int
	}{
		{
			name:      "Success: batch를 모두 저장 시도",
			batch:     []batchRetryTask{{msgs: newTestMsgs()}, {msgs: newTestMsgs()}},
			inChannel: 0,
			mockTimes: 2,
		},
		{
			name:      "Success: retryCh 잔여분도 함께 저장 시도",
			batch:     []batchRetryTask{{msgs: newTestMsgs()}},
			inChannel: 1,
			mockTimes: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockClient := mocks.NewMockChatServiceClient(t)
			mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
				Return(&emptypb.Empty{}, nil).Times(tt.mockTimes)

			a := newTestAdapter(t, mockClient)
			for range tt.inChannel {
				a.retryCh <- batchRetryTask{msgs: newTestMsgs()}
			}

			a.drainBatch(context.Background(), tt.batch)
		})
	}
}

func TestChatStoreAdapter_RunRetryWorker_ProcessesOnTick(t *testing.T) {
	t.Parallel()

	retried := make(chan struct{})
	mockClient := mocks.NewMockChatServiceClient(t)

	mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.Unavailable, "down")).Once()

	mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ *chatpb.BatchCreateMessagesRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
			close(retried)
			return &emptypb.Empty{}, nil
		}).Once()

	a := newChatStoreAdapter(mockClient, 1*time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go a.runRetryWorker(ctx)

	a.SaveMany(context.Background(), newTestMsgs())

	select {
	case <-retried:
	case <-time.After(3 * time.Second):
		t.Fatal("retry worker가 제시간에 재시도하지 않았다")
	}
}

func TestChatStoreAdapter_RunRetryWorker_GracefulDrain(t *testing.T) {
	t.Parallel()

	mockClient := mocks.NewMockChatServiceClient(t)
	mockClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil).Once()

	a := newTestAdapter(t, mockClient)

	a.retryCh <- batchRetryTask{msgs: newTestMsgs(), attempts: 1, nextRetry: time.Now()}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		a.runRetryWorker(ctx)
	}()

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("retry worker가 제시간에 종료되지 않았다")
	}

	mockClient.AssertExpectations(t)
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code      codes.Code
		retryable bool
	}{
		{codes.Unavailable, true},
		{codes.DeadlineExceeded, true},
		{codes.Internal, true},
		{codes.ResourceExhausted, true},
		{codes.AlreadyExists, false},
		{codes.InvalidArgument, false},
		{codes.NotFound, false},
		{codes.PermissionDenied, false},
		{codes.OK, false},
	}

	for _, tt := range tests {
		t.Run(tt.code.String(), func(t *testing.T) {
			t.Parallel()
			err := status.Error(tt.code, "test")
			assert.Equal(t, tt.retryable, isRetryable(err))
		})
	}
}

func TestChatStoreAdapter_JitteredBackoff(t *testing.T) {
	t.Parallel()

	a := newChatStoreAdapter(nil, time.Second)

	tests := []struct {
		attempts    int
		expectedMax time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second},
		{10, 30 * time.Second},
	}

	for _, tt := range tests {
		d := a.jitteredBackoff(tt.attempts)
		assert.GreaterOrEqual(t, d, time.Duration(1),
			"attempts=%d: backoff은 최소 1ns여야 한다", tt.attempts)
		assert.LessOrEqual(t, d, tt.expectedMax,
			"attempts=%d: backoff은 %v를 초과하면 안 된다", tt.attempts, tt.expectedMax)
	}
}
