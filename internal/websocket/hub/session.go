package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	maxMessageSize = 65536
	sendBufferSize = 250
)

type egressPacket struct {
	data       []byte
	senderID   string
	receivedAt time.Time
}

type sessionConfig struct {
	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration
	maxLength  int
}

type session struct {
	config   sessionConfig
	senderID string
	roomID   string

	conn *websocket.Conn

	unregisterCh chan<- *session
	broadcastCh  chan<- *Message
	sendCh       chan egressPacket
	allowFunc    func(userID, roomID string) bool

	mu     sync.RWMutex
	closed bool
}

func newSession(
	cfg sessionConfig,
	conn *websocket.Conn,
	senderID, roomID string,
	unregisterCh chan<- *session,
	broadcastCh chan<- *Message,
	allowFunc func(userID, roomID string) bool,
) *session {
	return &session{
		config:       cfg,
		senderID:     senderID,
		roomID:       roomID,
		conn:         conn,
		unregisterCh: unregisterCh,
		broadcastCh:  broadcastCh,
		sendCh:       make(chan egressPacket, sendBufferSize),
		allowFunc:    allowFunc,
	}
}

func (s *session) run(ctx context.Context) {
	connCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer cancel()
		s.readPump(connCtx)
	}()
	go func() {
		defer wg.Done()
		s.writePump(connCtx)
		s.conn.Close()
	}()

	wg.Wait()

	select {
	case s.unregisterCh <- s:
	case <-ctx.Done():
	}
}

func (s *session) readPump(ctx context.Context) {
	s.conn.SetReadLimit(maxMessageSize)
	s.conn.SetReadDeadline(time.Now().Add(s.config.pongWait))
	s.conn.SetPongHandler(func(string) error {
		s.conn.SetReadDeadline(time.Now().Add(s.config.pongWait))
		return nil
	})

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure,
			) {
				slog.WarnContext(ctx, "WebSocket unexpected close", "sender_id", s.senderID, "error", err)
			}
			return
		}

		messagesReceivedTotal.Add(ctx, 1)

		if s.allowFunc != nil && !s.allowFunc(s.senderID, s.roomID) {
			slog.WarnContext(ctx, "Rate limit exceeded for session (WS_MESSAGE Spam Protection)", "sender_id", s.senderID, "room_id", s.roomID)
			messagesRateLimitedTotal.Add(ctx, 1)
			s.sendSystemMessage(ctx, "rate limit exceeded: do not spam")
			continue
		}

		var req incomingRequest
		if err := json.Unmarshal(data, &req); err != nil {
			slog.WarnContext(ctx, "Invalid JSON message", "error", err, "sender_id", s.senderID, "msg_len", len(data))
			s.sendSystemMessage(ctx, "invalid message format")
			continue
		}

		if req.Content == "" || req.ClientMsgID == "" {
			slog.WarnContext(ctx, "Message validation failed: missing required fields",
				"sender_id", s.senderID, "content_len", len(req.Content))
			s.sendSystemMessage(ctx, "missing required fields: content, client_msg_id")
			continue
		}

		if s.config.maxLength > 0 && utf8.RuneCountInString(req.Content) > s.config.maxLength {
			slog.WarnContext(ctx, "Message content too long",
				"sender_id", s.senderID, "rune_count", utf8.RuneCountInString(req.Content), "max", s.config.maxLength)
			s.sendSystemMessage(ctx, fmt.Sprintf("message too long: max %d characters", s.config.maxLength))
			continue
		}

		if req.Type == "" {
			req.Type = msgTypeChat
		}

		now := time.Now()
		select {
		case s.broadcastCh <- &Message{
			RoomID:      s.roomID,
			SenderID:    s.senderID,
			Content:     req.Content,
			ClientMsgID: req.ClientMsgID,
			Type:        req.Type,
			Timestamp:   now.Unix(),
			ReceivedAt:  now,
		}:
		case <-ctx.Done():
			return
		}
	}
}

func (s *session) writePump(ctx context.Context) {
	ticker := time.NewTicker(s.config.pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case packet, ok := <-s.sendCh:
			s.conn.SetWriteDeadline(time.Now().Add(s.config.writeWait))
			if !ok {
				s.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if packet.senderID == s.senderID {
				observeEgress(ctx, packet.receivedAt)
			}

			if err := s.conn.WriteMessage(websocket.TextMessage, packet.data); err != nil {
				return
			}
			messagesSentTotal.Add(ctx, 1)

		case <-ticker.C:
			s.conn.SetWriteDeadline(time.Now().Add(s.config.writeWait))
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

func (s *session) sendSystemMessage(ctx context.Context, content string) {
	msg := &Message{
		Type:      msgTypeSystem,
		SenderID:  systemSenderID,
		RoomID:    s.roomID,
		Content:   content,
		Timestamp: time.Now().Unix(),
	}
	rawData, err := msg.toRawJSON()
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal system message", "error", err)
		return
	}
	s.send(ctx, rawData)
}

func (s *session) send(ctx context.Context, data []byte) {
	s.sendWithMeta(ctx, data, "", time.Time{})
}

func (s *session) sendWithMeta(ctx context.Context, data []byte, senderID string, receivedAt time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return
	}

	select {
	case s.sendCh <- egressPacket{data: data, senderID: senderID, receivedAt: receivedAt}:
		observeFanout(ctx, receivedAt)
	default:
		sendQueueDroppedTotal.Add(ctx, 1)
		slog.WarnContext(ctx, "Send queue full - dropping message", "sender_id", s.senderID)
	}
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	close(s.sendCh)
}
