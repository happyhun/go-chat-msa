//go:build integration

package websocket_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	natscontainer "github.com/testcontainers/testcontainers-go/modules/nats"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/wsticket"
	ws "go-chat-msa/internal/websocket"
	"go-chat-msa/internal/websocket/hub"
	"go-chat-msa/internal/websocket/natsbus"
)

const (
	testInternalSecret = "integration-secret"
	testAllowedOrigin  = "http://chat.example.test"
)

type wsNode struct {
	router *ws.Router
	bus    *natsbus.Bus
	server *httptest.Server
	wsURL  string
	cancel context.CancelFunc
	done   chan struct{}
}

type RouterIntegrationSuite struct {
	suite.Suite
	container      *natscontainer.NATSContainer
	natsURL        string
	mockUserClient *mocks.MockUserServiceClient
	redisClient    *redis.Client
	node           *wsNode
}

func (s *RouterIntegrationSuite) SetupSuite() {
	ctx := context.Background()

	container, err := natscontainer.Run(ctx, "nats:2.15-alpine", testcontainers.WithAlwaysPull(), natscontainer.WithConfigFile(strings.NewReader("port: 4222\njetstream { store_dir: /data/jetstream }\n")))
	s.Require().NoError(err)
	s.container = container

	url, err := container.ConnectionString(ctx)
	s.Require().NoError(err)
	s.natsURL = url
	nc, err := nats.Connect(url)
	s.Require().NoError(err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	s.Require().NoError(err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "CHAT_PERSIST", Subjects: []string{"chat.persist.*"}, Storage: jetstream.FileStorage,
		Retention: jetstream.WorkQueuePolicy, RePublish: &jetstream.RePublish{Source: "chat.persist.*", Destination: "room.msg.$1"},
	})
	s.Require().NoError(err)
}

func (s *RouterIntegrationSuite) TearDownSuite() {
	if s.container != nil {
		s.container.Terminate(context.Background())
	}
}

func (s *RouterIntegrationSuite) SetupTest() {
	s.mockUserClient = mocks.NewMockUserServiceClient(s.T())
	s.redisClient = redis.NewClient(&redis.Options{Addr: miniredis.RunT(s.T()).Addr()})
	s.node = s.startNode("pod-a")
}

func (s *RouterIntegrationSuite) TearDownTest() {
	s.stopNode(s.node)
	_ = s.redisClient.Close()
}

func (s *RouterIntegrationSuite) testConfig() ws.WebSocketConfig {
	return ws.WebSocketConfig{
		Manager: config.ManagerConfig{
			WriteWait:   10 * time.Second,
			PongWait:    60 * time.Second,
			PingPeriod:  54 * time.Second,
			IdleTimeout: 2 * time.Second,
		},
		GRPCClient: config.GRPCClientConfig{Timeout: time.Second},
		RateLimit: ws.RateLimitConfig{
			WSConnect: config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
			WSMessage: config.RateLimitConfig{RPS: 100, Burst: 100, TTL: time.Minute},
		},
		AllowedOrigins: []string{testAllowedOrigin},
		NATS: ws.NATSConfig{
			FlushTimeout:    2 * time.Second,
			MaxDeliveryLag:  5 * time.Second,
			SubPendingMsgs:  1000,
			SubPendingBytes: 8 * 1024 * 1024,
		},
	}
}

func (s *RouterIntegrationSuite) startNode(podName string) *wsNode {
	bus, err := natsbus.Connect(natsbus.Config{
		URL:             s.natsURL,
		Name:            podName,
		FlushTimeout:    2 * time.Second,
		SubPendingMsgs:  1000,
		SubPendingBytes: 8 * 1024 * 1024,
	})
	s.Require().NoError(err)

	router := ws.NewRouter(s.mockUserClient, s.testConfig(), bus,
		ws.WithPodName(podName),
		ws.WithInternalSecret(testInternalSecret),
		ws.WithRedisClient(s.redisClient))
	bus.SetObserver(router.Observer())

	server := httptest.NewServer(router)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.RunManager(ctx)
	}()

	return &wsNode{
		router: router,
		bus:    bus,
		server: server,
		wsURL:  strings.Replace(server.URL, "http", "ws", 1),
		cancel: cancel,
		done:   done,
	}
}

func (s *RouterIntegrationSuite) stopNode(node *wsNode) {
	if node == nil {
		return
	}
	node.server.Close()
	node.cancel()
	<-node.done
	_ = node.bus.Drain()
}

func (s *RouterIntegrationSuite) dial(node *wsNode, userID, roomID string) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.Dial(s.wsURL(node, userID, roomID), nil)
}

func (s *RouterIntegrationSuite) wsURL(node *wsNode, userID, roomID string) string {
	ticket, err := wsticket.NewStore(s.redisClient).Issue(context.Background(), userID, time.Minute)
	s.Require().NoError(err)
	return fmt.Sprintf("%s/ws?room_id=%s&ticket=%s", node.wsURL, roomID, ticket)
}

func (s *RouterIntegrationSuite) expectMember(times int) {
	s.mockUserClient.EXPECT().
		VerifyRoomMember(mock.Anything, mock.Anything).
		Return(&userpb.VerifyRoomMemberResponse{}, nil).Times(times)
}

func (s *RouterIntegrationSuite) readFrame(conn *websocket.Conn) map[string]any {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	s.Require().NoError(err)

	var frame map[string]any
	s.Require().NoError(json.Unmarshal(data, &frame))
	s.NotContains(frame, "frame_no")
	return frame
}

func (s *RouterIntegrationSuite) TestConnect_MissingTicket() {
	url := fmt.Sprintf("%s/ws?room_id=%s", s.node.wsURL, uuid.NewString())
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	s.Error(err)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

func (s *RouterIntegrationSuite) TestConnect_TicketIsSingleUse() {
	roomID := uuid.NewString()
	s.expectMember(1)

	url := s.wsURL(s.node, "user-1", roomID)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	s.Require().NoError(err)
	defer conn.Close()

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	s.Error(err)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

func (s *RouterIntegrationSuite) TestConnect_Origin() {
	tests := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{name: "Success: 허용 목록의 Origin", origin: testAllowedOrigin, wantStatus: http.StatusSwitchingProtocols},
		{name: "Success: Origin이 없는 비브라우저 클라이언트", wantStatus: http.StatusSwitchingProtocols},
		{name: "Failure: 허용되지 않은 Origin", origin: "http://evil.example.test", wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			s.expectMember(1)
			header := http.Header{}
			if tt.origin != "" {
				header.Set("Origin", tt.origin)
			}

			conn, resp, err := websocket.DefaultDialer.Dial(s.wsURL(s.node, "user-1", uuid.NewString()), header)
			if conn != nil {
				defer conn.Close()
			}
			if tt.wantStatus != http.StatusSwitchingProtocols {
				s.Error(err)
			}
			s.Equal(tt.wantStatus, resp.StatusCode)
		})
	}
}

func (s *RouterIntegrationSuite) TestConnect_InvalidRoomID() {
	_, resp, err := websocket.DefaultDialer.Dial(s.wsURL(s.node, "user-1", "room-1"), nil)
	s.Error(err)
	s.Equal(http.StatusBadRequest, resp.StatusCode)
}

func (s *RouterIntegrationSuite) TestConnect_NotRoomMember() {
	roomID := uuid.NewString()
	s.mockUserClient.EXPECT().
		VerifyRoomMember(mock.Anything, &userpb.VerifyRoomMemberRequest{
			RoomId: roomID,
			UserId: "user-outsider",
		}).Return(nil, status.Error(codes.NotFound, "not a member"))

	_, resp, err := websocket.DefaultDialer.Dial(s.wsURL(s.node, "user-outsider", roomID), nil)
	s.Error(err)
	s.Equal(http.StatusForbidden, resp.StatusCode)
}

func (s *RouterIntegrationSuite) TestPublish_EchoesThroughNATS() {
	roomID := uuid.NewString()
	userID := "user-1"
	s.expectMember(1)

	content := "Hello, World!"

	conn, resp, err := s.dial(s.node, userID, roomID)
	s.Require().NoError(err)
	s.Equal(http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()

	s.publish(roomID, userID, content, "msg-unique-id")

	frame := s.readFrame(conn)
	s.Equal("chat", frame["type"])
	s.Equal(content, frame["content"])
	s.Equal("msg-unique-id", frame["client_msg_id"])
	s.NotEmpty(frame["id"], "발행 시 UUIDv7 id가 부여된다")
}

func (s *RouterIntegrationSuite) TestBroadcast_AcrossPods() {
	roomID := uuid.NewString()
	s.expectMember(2)

	other := s.startNode("pod-b")
	defer s.stopNode(other)

	aliceConn, _, err := s.dial(s.node, "alice", roomID)
	s.Require().NoError(err)
	defer aliceConn.Close()

	bobConn, _, err := s.dial(other, "bob", roomID)
	s.Require().NoError(err)
	defer bobConn.Close()

	content := "cross pod"
	s.publish(roomID, "alice", content, "alice-msg-1")

	frame := s.readFrame(bobConn)
	s.Equal(content, frame["content"])
	s.Equal("alice", frame["sender_id"])
}

func (s *RouterIntegrationSuite) TestMultipleSessions_SameUser() {
	roomID := uuid.NewString()
	s.expectMember(3)

	first, _, err := s.dial(s.node, "same-user", roomID)
	s.Require().NoError(err)
	defer first.Close()

	second, _, err := s.dial(s.node, "same-user", roomID)
	s.Require().NoError(err)
	defer second.Close()

	sender, _, err := s.dial(s.node, "other-user", roomID)
	s.Require().NoError(err)
	defer sender.Close()

	s.publish(roomID, "other-user", "hi both tabs", "multi-1")

	for _, conn := range []*websocket.Conn{first, second} {
		frame := s.readFrame(conn)
		s.Equal("hi both tabs", frame["content"])
	}
}

func (s *RouterIntegrationSuite) TestInternal_SystemMessage() {
	roomID := uuid.NewString()
	s.expectMember(1)

	conn, _, err := s.dial(s.node, "connected-user", roomID)
	s.Require().NoError(err)
	defer conn.Close()

	req := httptest.NewRequest(http.MethodPost, "/internal/rooms/"+roomID+"/system-messages",
		strings.NewReader(`{"username":"tester","event":"join"}`))
	req.Header.Set("X-Internal-Secret", testInternalSecret)
	w := httptest.NewRecorder()

	s.node.router.ServeHTTP(w, req)
	s.Equal(http.StatusNoContent, w.Code)

	frame := s.readFrame(conn)
	s.Equal("system", frame["type"])
	s.Contains(frame["content"], "tester")
	s.Contains(frame["content"], "들어왔습니다")
}

func (s *RouterIntegrationSuite) TestInternal_CloseRoomSessionsAcrossPods() {
	roomID := uuid.NewString()
	s.expectMember(1)

	other := s.startNode("pod-b")
	defer s.stopNode(other)

	conn, _, err := s.dial(other, "victim", roomID)
	s.Require().NoError(err)
	defer conn.Close()

	req := httptest.NewRequest(http.MethodDelete, "/internal/rooms/"+roomID+"/sessions", nil)
	req.Header.Set("X-Internal-Secret", testInternalSecret)
	w := httptest.NewRecorder()

	s.node.router.ServeHTTP(w, req)
	s.Equal(http.StatusNoContent, w.Code)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	s.Require().ErrorAs(err, &closeErr)
	s.Equal(websocket.CloseTryAgainLater, closeErr.Code)
}

func (s *RouterIntegrationSuite) TestInternal_RequiresSecret() {
	req := httptest.NewRequest(http.MethodDelete, "/internal/rooms/"+uuid.NewString()+"/sessions", nil)
	w := httptest.NewRecorder()

	s.node.router.ServeHTTP(w, req)
	s.Equal(http.StatusUnauthorized, w.Code)
}

func (s *RouterIntegrationSuite) publish(roomID, senderID, content, clientMsgID string) {
	id, err := uuid.NewV7()
	s.Require().NoError(err)
	payload, err := json.Marshal(map[string]any{
		"id": id.String(), "room_id": roomID, "sender_id": senderID,
		"content": content, "client_msg_id": clientMsgID, "type": "chat", "timestamp": time.Now().Unix(),
	})
	s.Require().NoError(err)
	s.Require().NoError(s.node.bus.PublishMessage(s.T().Context(), hub.Envelope{
		RoomID: roomID, SenderID: senderID, MessageID: id.String(), Payload: payload,
		ReceivedAt: time.Now(), OriginPod: "chat-pod",
	}))
}

func (s *RouterIntegrationSuite) TestSend_ViaWebSocket() {
	s.expectMember(1)
	roomID := uuid.NewString()
	conn, _, err := s.dial(s.node, "sender", roomID)
	s.Require().NoError(err)
	defer conn.Close()
	clientMsgID := uuid.NewString()
	s.Require().NoError(conn.WriteJSON(map[string]string{"content": "accepted", "client_msg_id": clientMsgID}))
	frame := s.readFrame(conn)
	s.Equal("chat", frame["type"])
	s.Equal("accepted", frame["content"])
	s.Equal(clientMsgID, frame["client_msg_id"])
	s.NotEmpty(frame["id"])
}

func TestRouterIntegrationSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	suite.Run(t, new(RouterIntegrationSuite))
}
