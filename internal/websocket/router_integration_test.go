//go:build integration

package websocket_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/shared/config"
	ws "go-chat-msa/internal/websocket"
)

type RouterIntegrationSuite struct {
	suite.Suite
	mockUserClient *mocks.MockUserServiceClient
	mockChatClient *mocks.MockChatServiceClient
	router         *ws.Router
	server         *httptest.Server
	wsURL          string
	cancel         context.CancelFunc
	managerDone    chan struct{}
}

func (s *RouterIntegrationSuite) SetupTest() {
	s.mockUserClient = mocks.NewMockUserServiceClient(s.T())
	s.mockChatClient = mocks.NewMockChatServiceClient(s.T())

	cfg := ws.WebSocketConfig{
		Manager: config.ManagerConfig{
			WriteWait:   10 * time.Second,
			PongWait:    60 * time.Second,
			PingPeriod:  54 * time.Second,
			IdleTimeout: 100 * time.Millisecond,
		},
	}

	s.router = ws.NewRouter(s.mockChatClient, s.mockUserClient, cfg)
	s.server = httptest.NewServer(s.router)
	s.wsURL = strings.Replace(s.server.URL, "http", "ws", 1)

	ctx, cancel := context.WithCancel(s.T().Context())
	s.cancel = cancel
	s.managerDone = make(chan struct{})
	go func() {
		defer close(s.managerDone)
		s.router.RunManager(ctx)
	}()
}

func (s *RouterIntegrationSuite) TearDownTest() {
	s.server.Close()
	s.cancel()
	<-s.managerDone
}

func (s *RouterIntegrationSuite) dial(userID, roomID string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	header.Set("X-User-ID", userID)
	url := fmt.Sprintf("%s/ws?room_id=%s", s.wsURL, roomID)
	return websocket.DefaultDialer.Dial(url, header)
}

func (s *RouterIntegrationSuite) TestConnect_MissingUserID() {
	header := http.Header{}
	url := fmt.Sprintf("%s/ws?room_id=room-1", s.wsURL)
	_, resp, err := websocket.DefaultDialer.Dial(url, header)
	s.Error(err)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

func (s *RouterIntegrationSuite) TestConnect_MissingRoomID() {
	header := http.Header{}
	header.Set("X-User-ID", "user-1")
	url := fmt.Sprintf("%s/ws", s.wsURL)
	_, resp, err := websocket.DefaultDialer.Dial(url, header)
	s.Error(err)
	s.Equal(http.StatusBadRequest, resp.StatusCode)
}

func (s *RouterIntegrationSuite) TestConnect_NotRoomMember() {
	s.mockUserClient.EXPECT().
		VerifyRoomMember(mock.Anything, &userpb.VerifyRoomMemberRequest{
			RoomId: "room-forbidden",
			UserId: "user-outsider",
		}).Return(nil, status.Error(codes.NotFound, "not a member"))

	header := http.Header{}
	header.Set("X-User-ID", "user-outsider")
	url := fmt.Sprintf("%s/ws?room_id=room-forbidden", s.wsURL)
	_, resp, err := websocket.DefaultDialer.Dial(url, header)
	s.Error(err)
	s.Equal(http.StatusForbidden, resp.StatusCode)
}

func (s *RouterIntegrationSuite) TestConnect_Success() {
	userID := "user-1"
	roomID := "room-1"

	s.mockUserClient.EXPECT().
		VerifyRoomMember(mock.Anything, &userpb.VerifyRoomMemberRequest{
			RoomId: roomID,
			UserId: userID,
		}).Return(&userpb.VerifyRoomMemberResponse{}, nil)

	s.mockChatClient.EXPECT().
		GetLastSequenceNumber(mock.Anything, &chatpb.GetLastSequenceNumberRequest{
			RoomId: roomID,
		}).Return(&chatpb.GetLastSequenceNumberResponse{SequenceNumber: 100}, nil)

	conn, resp, err := s.dial(userID, roomID)
	s.Require().NoError(err)
	s.Equal(http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()

	content := "Hello, World!"
	clientMsgID := "msg-unique-id"

	s.mockChatClient.EXPECT().
		BatchCreateMessages(mock.Anything, mock.MatchedBy(func(req *chatpb.BatchCreateMessagesRequest) bool {
			if len(req.Requests) == 0 {
				return false
			}
			r := req.Requests[0]
			return r.Content == content && r.RoomId == roomID && r.SenderId == userID && r.SequenceNumber == 101
		})).Return(&emptypb.Empty{}, nil)

	msgReq := map[string]string{
		"type":          "chat",
		"content":       content,
		"client_msg_id": clientMsgID,
	}
	err = conn.WriteJSON(msgReq)
	s.Require().NoError(err)

	var received map[string]interface{}
	err = conn.ReadJSON(&received)
	s.Require().NoError(err)

	s.Equal("chat", received["type"])
	s.Equal(content, received["content"])
	s.Equal(float64(101), received["sequence_number"])
	s.Equal(clientMsgID, received["client_msg_id"])
}

func (s *RouterIntegrationSuite) TestBroadcast_BetweenClients() {
	roomID := "broadcast-room"
	aliceID := "alice"
	bobID := "bob"

	s.mockChatClient.EXPECT().
		GetLastSequenceNumber(mock.Anything, &chatpb.GetLastSequenceNumberRequest{
			RoomId: roomID,
		}).Return(&chatpb.GetLastSequenceNumberResponse{SequenceNumber: 0}, nil)

	s.mockUserClient.EXPECT().
		VerifyRoomMember(mock.Anything, mock.MatchedBy(func(req *userpb.VerifyRoomMemberRequest) bool {
			return req.RoomId == roomID
		})).Return(&userpb.VerifyRoomMemberResponse{}, nil).Twice()

	aliceConn, _, err := s.dial(aliceID, roomID)
	s.Require().NoError(err)
	defer aliceConn.Close()

	bobConn, _, err := s.dial(bobID, roomID)
	s.Require().NoError(err)
	defer bobConn.Close()

	content := "Hi Bob!"
	s.mockChatClient.EXPECT().
		BatchCreateMessages(mock.Anything, mock.MatchedBy(func(req *chatpb.BatchCreateMessagesRequest) bool {
			if len(req.Requests) == 0 {
				return false
			}
			r := req.Requests[0]
			return r.Content == content && r.RoomId == roomID && r.SenderId == aliceID
		})).Return(&emptypb.Empty{}, nil)

	err = aliceConn.WriteJSON(map[string]string{
		"type":          "chat",
		"content":       content,
		"client_msg_id": "alice-msg-1",
	})
	s.Require().NoError(err)

	var bobReceived map[string]interface{}
	err = bobConn.ReadJSON(&bobReceived)
	s.Require().NoError(err)
	s.Equal(content, bobReceived["content"])
	s.Equal(aliceID, bobReceived["sender_id"])
}

func (s *RouterIntegrationSuite) TestSession_Conflict_Kick() {
	userID := "conflicting-user"
	roomID := "conflict-room"

	s.mockUserClient.EXPECT().VerifyRoomMember(mock.Anything, mock.Anything).
		Return(&userpb.VerifyRoomMemberResponse{}, nil).Twice()
	s.mockChatClient.EXPECT().GetLastSequenceNumber(mock.Anything, mock.Anything).
		Return(&chatpb.GetLastSequenceNumberResponse{SequenceNumber: 0}, nil)

	conn1, _, err := s.dial(userID, roomID)
	s.Require().NoError(err)
	defer conn1.Close()

	conn2, _, err := s.dial(userID, roomID)
	s.Require().NoError(err)
	defer conn2.Close()

	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg map[string]interface{}
	err = conn1.ReadJSON(&msg)
	s.Require().NoError(err, "Should receive conflict message")
	s.Equal("conflict", msg["type"])

	_, _, err = conn1.ReadMessage()
	s.Error(err, "Connection should be closed")
}

func (s *RouterIntegrationSuite) TestInternal_Broadcast() {
	userID := "connected-user"
	roomID := "internal-broadcast-room"

	s.mockUserClient.EXPECT().VerifyRoomMember(mock.Anything, mock.Anything).
		Return(&userpb.VerifyRoomMemberResponse{}, nil)
	s.mockChatClient.EXPECT().GetLastSequenceNumber(mock.Anything, mock.Anything).
		Return(&chatpb.GetLastSequenceNumberResponse{SequenceNumber: 0}, nil)
	s.mockChatClient.EXPECT().BatchCreateMessages(mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil)

	conn, _, err := s.dial(userID, roomID)
	s.Require().NoError(err)
	defer conn.Close()

	time.Sleep(20 * time.Millisecond)

	username := "tester"
	reqBody := fmt.Sprintf(`{"username":"%s","event":"join"}`, username)
	req := httptest.NewRequest("POST", "/internal/rooms/"+roomID+"/broadcast", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	s.router.ServeHTTP(w, req)
	s.Equal(http.StatusNoContent, w.Code)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var received map[string]interface{}
	err = conn.ReadJSON(&received)
	s.Require().NoError(err, "Should receive internal broadcast message")
	s.Equal("system", received["type"])
	s.Contains(received["content"], username)
	s.Contains(received["content"], "들어왔습니다")
}

func TestRouterIntegrationSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	suite.Run(t, new(RouterIntegrationSuite))
}
