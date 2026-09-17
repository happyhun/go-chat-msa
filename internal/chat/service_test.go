package chat_test

import (
	"errors"
	"testing"
	"time"

	pb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/chat"
	"go-chat-msa/internal/chat/mocks"
	"go-chat-msa/internal/shared/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
				m.EXPECT().GetHistory(mock.Anything, "r1", int64(11), mock.Anything).Return([]*chat.Message{
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
				m.EXPECT().GetHistory(mock.Anything, "r1", int64(51), mock.Anything).Return(nil, nil)
			},
			req:           &pb.ListMessagesRequest{RoomId: "r1", Limit: 0},
			wantLen:       0,
			expectedLimit: 50,
			wantErr:       false,
		},
		{
			name: "Success: 최대 limit 상한 적용",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetHistory(mock.Anything, "r1", int64(101), mock.Anything).Return(nil, nil)
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
			name: "Failure: 저장소 장애 (Unavailable)",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().GetHistory(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.New("db fail"))
			},
			req:     &pb.ListMessagesRequest{RoomId: "r1"},
			wantErr: true,
			code:    codes.Unavailable,
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

	const afterID = "01920f6a-7c3e-7b1a-9d2f-3e4a5b6c7d8e"

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
			name: "Success: 기본 limit으로 처음부터 동기화",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SyncMessages(mock.Anything, "r1", "", int64(16), mock.Anything).Return(nil, nil)
			},
			req:     &pb.SyncMessagesRequest{RoomId: "r1", Limit: 0},
			wantErr: false,
		},
		{
			name: "Success: 최대 limit 상한을 적용하여 메시지 동기화",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SyncMessages(mock.Anything, "r1", "", int64(101), mock.Anything).Return(nil, nil)
			},
			req:     &pb.SyncMessagesRequest{RoomId: "r1", Limit: 500},
			wantErr: false,
		},
		{
			name: "Success: after_message_id 기준으로 동기화",
			mock: func(m *mocks.MockRepository) {
				m.EXPECT().SyncMessages(mock.Anything, "r1", afterID, int64(16), mock.Anything).Return(nil, nil)
			},
			req:     &pb.SyncMessagesRequest{RoomId: "r1", AfterMessageId: afterID},
			wantErr: false,
		},
		{
			name:    "Failure: after_message_id 형식 오류 (InvalidArgument)",
			mock:    func(m *mocks.MockRepository) {},
			req:     &pb.SyncMessagesRequest{RoomId: "r1", AfterMessageId: "not-a-uuid"},
			wantErr: true,
			code:    codes.InvalidArgument,
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

func TestService_SyncMessagesHasMore(t *testing.T) {
	t.Parallel()

	cfg := config.ChatConfig{
		Sync: config.SyncConfig{DefaultLimit: 2, MaxLimit: 100},
	}

	repo := mocks.NewMockRepository(t)
	repo.EXPECT().SyncMessages(mock.Anything, "r1", "", int64(3), mock.Anything).Return([]*chat.Message{
		{ID: "m1"}, {ID: "m2"}, {ID: "m3"},
	}, nil)

	s := chat.NewService(repo, cfg)
	res, err := s.SyncMessages(t.Context(), &pb.SyncMessagesRequest{RoomId: "r1"})

	require.NoError(t, err)
	assert.Len(t, res.Messages, 2, "limit + 1로 조회하고 limit개만 반환한다")
	assert.True(t, res.HasMore)
}
