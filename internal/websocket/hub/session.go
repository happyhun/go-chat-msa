package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	maxMessageSize = 65536
	sendBufferSize = 250

	transientUnavailableMsg = "message temporarily unavailable, please retry"
)

type egressPacket struct {
	data       []byte
	frameNo    int64
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
	id       string
	config   sessionConfig
	senderID string
	roomID   string

	conn *websocket.Conn

	unregisterCh chan<- *session
	publishFunc  publishFunc
	sendCh       chan egressPacket
	allowFunc    func(userID, roomID string) bool

	frameNo  int64
	frameBuf []byte

	mu          sync.RWMutex
	closed      bool
	closeCode   int
	closeReason string
}

func newSession(
	id string,
	cfg sessionConfig,
	conn *websocket.Conn,
	senderID, roomID string,
	unregisterCh chan<- *session,
	publish publishFunc,
	allowFunc func(userID, roomID string) bool,
) *session {
	return &session{
		id:           id,
		config:       cfg,
		senderID:     senderID,
		roomID:       roomID,
		conn:         conn,
		unregisterCh: unregisterCh,
		publishFunc:  publish,
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
		_ = s.conn.Close()
	}()

	wg.Wait()

	select {
	case s.unregisterCh <- s:
	case <-ctx.Done():
	}
}

func (s *session) readPump(ctx context.Context) {
	s.conn.SetReadLimit(maxMessageSize)
	if err := s.conn.SetReadDeadline(time.Now().Add(s.config.pongWait)); err != nil {
		return
	}
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(s.config.pongWait))
	})
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.WarnContext(ctx, "WebSocket unexpected close", "sender_id", s.senderID, "error", err)
			}
			return
		}

		messagesReceivedTotal.Add(ctx, 1)
		if s.allowFunc != nil && !s.allowFunc(s.senderID, s.roomID) {
			messagesRateLimitedTotal.Add(ctx, 1)
			s.sendSystemMessage(ctx, "rate limit exceeded: do not spam")
			continue
		}

		var req incomingRequest
		if err := json.Unmarshal(data, &req); err != nil {
			s.sendSystemMessage(ctx, "invalid message format")
			continue
		}
		if req.Content == "" || req.ClientMsgID == "" {
			s.sendSystemMessage(ctx, "missing required fields: content, client_msg_id")
			continue
		}
		clientMsgID, err := uuid.Parse(req.ClientMsgID)
		if err != nil || clientMsgID.String() != req.ClientMsgID {
			s.sendSystemMessage(ctx, "client_msg_id must be a canonical UUID")
			continue
		}
		if s.config.maxLength > 0 && utf8.RuneCountInString(req.Content) > s.config.maxLength {
			s.sendSystemMessage(ctx, fmt.Sprintf("message too long: max %d characters", s.config.maxLength))
			continue
		}
		if req.Type == "" {
			req.Type = msgTypeChat
		}
		if req.Type != msgTypeChat {
			s.sendSystemMessage(ctx, "invalid message type")
			continue
		}
		if s.publishFunc == nil {
			return
		}

		now := time.Now()
		err = s.publishFunc(ctx, &Message{
			RoomID:      s.roomID,
			SenderID:    s.senderID,
			Content:     req.Content,
			ClientMsgID: req.ClientMsgID,
			Type:        req.Type,
			Timestamp:   now.Unix(),
			ReceivedAt:  now,
		})
		if err == nil {
			continue
		}
		if errors.Is(err, ErrBusUnavailable) {
			s.sendSystemMessage(ctx, transientUnavailableMsg)
			return
		}
		return
	}
}

func (s *session) sendSystemMessage(ctx context.Context, content string) {
	msg := &Message{RoomID: s.roomID, SenderID: systemSenderID, Content: content, Type: msgTypeSystem, Timestamp: time.Now().Unix()}
	data, err := msg.toRawJSON()
	if err != nil {
		return
	}
	s.send(ctx, egressPacket{data: data, senderID: systemSenderID})
}

func (s *session) writePump(ctx context.Context) {
	ticker := time.NewTicker(s.config.pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case packet, ok := <-s.sendCh:
			if err := s.conn.SetWriteDeadline(time.Now().Add(s.config.writeWait)); err != nil {
				return
			}
			if !ok {
				_ = s.conn.WriteMessage(websocket.CloseMessage, s.closeMessage())
				return
			}

			if packet.senderID == s.senderID {
				observeEgress(ctx, packet.receivedAt)
			}

			if err := s.writeFrame(packet); err != nil {
				return
			}
			messagesSentTotal.Add(ctx, 1)

		case <-ticker.C:
			if err := s.conn.SetWriteDeadline(time.Now().Add(s.config.writeWait)); err != nil {
				return
			}
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-ctx.Done():
			if s.isClosed() {
				if err := s.conn.SetWriteDeadline(time.Now().Add(s.config.writeWait)); err != nil {
					return
				}
				_ = s.conn.WriteMessage(websocket.CloseMessage, s.closeMessage())
			}
			return
		}
	}
}

func (s *session) writeFrame(packet egressPacket) error {
	end := bytes.LastIndexByte(packet.data, '}')
	if end < 0 {
		return s.conn.WriteMessage(websocket.TextMessage, packet.data)
	}

	s.frameBuf = appendFrameNoSuffix(append(s.frameBuf[:0], packet.data[:end]...), packet.frameNo)
	return s.conn.WriteMessage(websocket.TextMessage, s.frameBuf)
}

func (s *session) send(ctx context.Context, packet egressPacket) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.frameNo++
	packet.frameNo = s.frameNo
	dropped := false

	select {
	case s.sendCh <- packet:
	default:
		dropped = true
	}
	s.mu.Unlock()

	if dropped {
		sendQueueDroppedTotal.Add(ctx, 1)
		slog.WarnContext(ctx, "Send queue full - dropping frame", "sender_id", s.senderID, "session_id", s.id)
	}
}

func (s *session) close() {
	s.closeWithCode(0, "")
}

func (s *session) closeWithCode(code int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true
	s.closeCode = code
	s.closeReason = reason

	close(s.sendCh)
}

func (s *session) closeMessage() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closeCode == 0 {
		return []byte{}
	}
	return websocket.FormatCloseMessage(s.closeCode, s.closeReason)
}

func (s *session) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
