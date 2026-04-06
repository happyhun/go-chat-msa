package chat_test

import (
	"errors"
	"testing"
	"time"

	pb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/chat"
	"go-chat-msa/internal/chat/mocks"
	"go-chat-msa/internal/shared/config"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestService_BatchCreateMessages(t *testing.T) {
	t.Parallel()

	cfg := config.ChatConfig{
		Message: config.MessageConfig{
			MaxLength: 10,
		},
		History: config.HistoryConfig{
			MaxLimit:     1000,
			DefaultLimit: 100,
		},
		Sync: config.SyncConfig{
			DefaultLimit: 50,
		},
	}

	tests := []struct {
		name    string
		mock    func(m *mocks.MockRepository)
		reqs    []*pb.CreateMessageRequest
		wantErr bool
		code    codes.Code
	}{
		{
			name: "Success: 메시지 정상 저장",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SaveMany(mock.Anything, mock.Anything).Return(nil)
			},
			reqs: []*pb.CreateMessageRequest{{
				RoomId:      "room_1",
				SenderId:    "user_1",
				Content:     "Hello",
				ClientMsgId: "msg_123",
			}},
		},
		{
			name: "Success: 커스텀 메시지 ID(UUID v7)와 함께 저장",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SaveMany(mock.Anything, mock.MatchedBy(func(msgs []*chat.Message) bool {
					return len(msgs) == 1 && msgs[0].ID != "" && !msgs[0].CreatedAt.IsZero()
				})).Return(nil)
			},
			reqs: []*pb.CreateMessageRequest{{
				RoomId:    "r1",
				SenderId:  "u1",
				Content:   "hi",
				MessageId: uuid.NewString(),
			}},
		},
		{
			name: "Success: 빈 요청은 즉시 성공",
			mock: func(m *mocks.MockRepository) {},
			reqs: nil,
		},
		{
			name: "Failure: 필수 필드 누락 (InvalidArgument)",
			mock: func(m *mocks.MockRepository) {},
			reqs: []*pb.CreateMessageRequest{{
				RoomId:   "",
				SenderId: "user_1",
				Content:  "Hello",
			}},
			wantErr: true,
			code:    codes.InvalidArgument,
		},
		{
			name: "Failure: 메시지 길이 초과 (InvalidArgument)",
			mock: func(m *mocks.MockRepository) {},
			reqs: []*pb.CreateMessageRequest{{
				RoomId:   "r1",
				SenderId: "u1",
				Content:  "this is way too long",
			}},
			wantErr: true,
			code:    codes.InvalidArgument,
		},
		{
			name: "Failure: 저장소 내부 에러 (Internal)",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SaveMany(mock.Anything, mock.Anything).Return(errors.New("db error"))
			},
			reqs: []*pb.CreateMessageRequest{{
				RoomId:   "room_1",
				SenderId: "user_1",
				Content:  "Hello",
			}},
			wantErr: true,
			code:    codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := mocks.NewMockRepository(t)
			tt.mock(repo)
			s := chat.NewService(repo, cfg)
			res, err := s.BatchCreateMessages(t.Context(), &pb.BatchCreateMessagesRequest{Requests: tt.reqs})

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.code, status.Code(err))
				assert.Nil(t, res)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, res)
			}
		})
	}
}

func TestService_ListMessages(t *testing.T) {
	t.Parallel()

	cfg := config.ChatConfig{
		History: config.HistoryConfig{
			MaxLimit:     100,
			DefaultLimit: 50,
		},
	}

	tests := []struct {
		name          string
		mock          func(m *mocks.MockRepository)
		req           *pb.ListMessagesRequest
		wantLen       int
		expectedLimit int64
		wantErr       bool
		code          codes.Code
	}{
		{
			name: "Success: 대화 내역 정상 조회",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetHistory(mock.Anything, "r1", int64(10), mock.Anything).Return([]*chat.Message{
					{ID: "m1", Content: "hello", CreatedAt: time.Now()},
				}, nil)
			},
			req:           &pb.ListMessagesRequest{RoomId: "r1", Limit: 10},
			wantLen:       1,
			expectedLimit: 10,
			wantErr:       false,
			code:          codes.OK,
		},
		{
			name: "Success: 기본 limit 사용",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetHistory(mock.Anything, "r1", int64(50), mock.Anything).Return(nil, nil)
			},
			req:           &pb.ListMessagesRequest{RoomId: "r1", Limit: 0},
			wantLen:       0,
			expectedLimit: 50,
			wantErr:       false,
		},
		{
			name: "Success: 최대 limit 상한 적용",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetHistory(mock.Anything, "r1", int64(100), mock.Anything).Return(nil, nil)
			},
			req:           &pb.ListMessagesRequest{RoomId: "r1", Limit: 500},
			wantLen:       0,
			expectedLimit: 100,
			wantErr:       false,
		},
		{
			name:    "Failure: 룸 ID 누락 (InvalidArgument)",
			mock:    func(m *mocks.MockRepository) {},
			req:     &pb.ListMessagesRequest{RoomId: ""},
			wantErr: true,
			code:    codes.InvalidArgument,
		},
		{
			name: "Failure: 저장소 내부 에러 (Internal)",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetHistory(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.New("db fail"))
			},
			req:     &pb.ListMessagesRequest{RoomId: "r1"},
			wantErr: true,
			code:    codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := mocks.NewMockRepository(t)
			tt.mock(repo)
			s := chat.NewService(repo, cfg)
			res, err := s.ListMessages(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.code, status.Code(err))
			} else {
				require.NoError(t, err)
				assert.Len(t, res.Messages, tt.wantLen)
			}
		})
	}
}

func TestService_SyncMessages(t *testing.T) {
	t.Parallel()

	cfg := config.ChatConfig{
		Sync: config.SyncConfig{
			DefaultLimit: 15,
			MaxLimit:     100,
		},
	}

	tests := []struct {
		name          string
		mock          func(m *mocks.MockRepository)
		req           *pb.SyncMessagesRequest
		expectedLimit int64
		wantErr       bool
		code          codes.Code
	}{
		{
			name: "Success: 기본 limit으로 메시지 동기화",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SyncMessages(mock.Anything, "r1", mock.Anything, int64(15), mock.Anything).Return(nil, nil)
			},
			req:     &pb.SyncMessagesRequest{RoomId: "r1", Limit: 0},
			wantErr: false,
		},
		{
			name: "Success: 최대 limit 상한을 적용하여 메시지 동기화",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SyncMessages(mock.Anything, "r1", mock.Anything, int64(100), mock.Anything).Return(nil, nil)
			},
			req:     &pb.SyncMessagesRequest{RoomId: "r1", Limit: 500},
			wantErr: false,
		},
		{
			name:    "Failure: 룸 ID 누락 (InvalidArgument)",
			mock:    func(m *mocks.MockRepository) {},
			req:     &pb.SyncMessagesRequest{RoomId: ""},
			wantErr: true,
			code:    codes.InvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := mocks.NewMockRepository(t)
			tt.mock(repo)
			s := chat.NewService(repo, cfg)
			_, err := s.SyncMessages(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.code, status.Code(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestService_GetLastSequenceNumber(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mock    func(m *mocks.MockRepository)
		req     *pb.GetLastSequenceNumberRequest
		wantErr bool
		code    codes.Code
	}{
		{
			name: "Success: 마지막 시퀀스 번호 조회",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetLastSequenceNumber(mock.Anything, "r1").Return(int64(10), nil)
			},
			req:     &pb.GetLastSequenceNumberRequest{RoomId: "r1"},
			wantErr: false,
		},
		{
			name: "Failure: 저장소 내부 에러 (Internal)",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetLastSequenceNumber(mock.Anything, "r1").Return(0, errors.New("db error"))
			},
			req:     &pb.GetLastSequenceNumberRequest{RoomId: "r1"},
			wantErr: true,
			code:    codes.Internal,
		},
		{
			name:    "Failure: 룸 ID 누락 (InvalidArgument)",
			mock:    func(m *mocks.MockRepository) {},
			req:     &pb.GetLastSequenceNumberRequest{RoomId: ""},
			wantErr: true,
			code:    codes.InvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := mocks.NewMockRepository(t)
			tt.mock(repo)
			s := chat.NewService(repo, config.ChatConfig{})
			res, err := s.GetLastSequenceNumber(t.Context(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.code, status.Code(err))
			} else {
				require.NoError(t, err)
				assert.NotNil(t, res)
			}
		})
	}
}
