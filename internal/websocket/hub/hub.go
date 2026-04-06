package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	lru "github.com/hashicorp/golang-lru/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	systemSenderID       = "system"
	idempotencyCacheSize = 250

	hubUnregisterBufferSize = 50
	hubBroadcastBufferSize  = 250
)

type registerHubReq struct {
	conn   *websocket.Conn
	userID string
	errCh  chan error
}

type MessageStore interface {
	GetLastSequenceNumber(ctx context.Context, roomID string) (int64, error)
	SaveMany(ctx context.Context, msgs []*Message)
}

type Hub struct {
	roomID      string
	sessionCfg  sessionConfig
	idleTimeout time.Duration
	store       MessageStore
	allowFunc   func(userID, roomID string) bool

	sessions         map[string]*session
	lastSequence     atomic.Int64
	idempotencyCache *lru.Cache[string, *Message]

	registerCh   chan registerHubReq
	unregisterCh chan *session
	broadcastCh  chan *Message
	persistCh    chan<- *Message

	doneCh   chan struct{}
	stopCh   chan struct{}
	stopOnce sync.Once
}

func newHub(
	roomID string,
	sessionCfg sessionConfig,
	idleTimeout time.Duration,
	store MessageStore,
	persistCh chan<- *Message,
	allowFunc func(userID, roomID string) bool,
) *Hub {
	cache, err := lru.New[string, *Message](idempotencyCacheSize)
	if err != nil {
		panic(fmt.Sprintf("hub: failed to create LRU cache: %v", err))
	}
	return &Hub{
		roomID:           roomID,
		sessionCfg:       sessionCfg,
		idleTimeout:      idleTimeout,
		store:            store,
		allowFunc:        allowFunc,
		sessions:         make(map[string]*session),
		idempotencyCache: cache,
		registerCh:       make(chan registerHubReq),
		unregisterCh:     make(chan *session, hubUnregisterBufferSize),
		broadcastCh:      make(chan *Message, hubBroadcastBufferSize),
		persistCh:        persistCh,
		doneCh:           make(chan struct{}),
		stopCh:           make(chan struct{}),
	}
}

func (h *Hub) run(ctx context.Context) {
	sessionCtx, cancelSessions := context.WithCancel(ctx)

	slog.InfoContext(ctx, "Hub actor started", "room_id", h.roomID)
	defer func() {
		slog.InfoContext(ctx, "Hub actor stopped", "room_id", h.roomID)
		cancelSessions()
		close(h.doneCh)
		h.shutdown()
	}()

	h.initializeSequence(ctx)

	idleTimer := time.NewTimer(h.idleTimeout)
	idleTimer.Stop()

	for {
		select {
		case req := <-h.registerCh:
			s := newSession(h.sessionCfg, req.conn, req.userID, h.roomID, h.unregisterCh, h.broadcastCh, h.allowFunc)
			h.registerSession(ctx, s, idleTimer)
			go s.run(sessionCtx)
			req.errCh <- nil

		case s := <-h.unregisterCh:
			if currentSession, ok := h.sessions[s.senderID]; ok && currentSession == s {
				delete(h.sessions, s.senderID)
				s.close()
				connectionsActive.Add(ctx, -1)
			}

			if len(h.sessions) == 0 {
				idleTimer.Reset(h.idleTimeout)
				slog.InfoContext(ctx, "Hub is empty, starting idle timer", "room_id", h.roomID)
			}

		case <-idleTimer.C:
			slog.InfoContext(ctx, "Hub idle timeout reached, shutting down", "room_id", h.roomID)
			hubsClosedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "idle")))
			return

		case message := <-h.broadcastCh:
			broadcastChannelDepth.Record(ctx, float64(len(h.broadcastCh)))

			if message.ClientMsgID != "" {
				if _, exists := h.idempotencyCache.Get(message.ClientMsgID); exists {
					slog.InfoContext(ctx, "Duplicate message dropped (idempotency)", "client_msg_id", message.ClientMsgID, "room_id", h.roomID)
					duplicateMessagesDroppedTotal.Add(ctx, 1)
					continue
				}
			}

			h.fanOut(ctx, message)

			if message.ClientMsgID != "" {
				h.idempotencyCache.Add(message.ClientMsgID, message)
			}

		case <-ctx.Done():
			return

		case <-h.stopCh:
			slog.InfoContext(ctx, "Hub stopped by manager command", "room_id", h.roomID)
			hubsClosedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "force")))
			return
		}
	}
}

func (h *Hub) initializeSequence(ctx context.Context) {
	if h.store == nil {
		return
	}
	seq, err := h.store.GetLastSequenceNumber(ctx, h.roomID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize sequence number", "room_id", h.roomID, "error", err)
		return
	}
	h.lastSequence.Store(seq)
	slog.InfoContext(ctx, "Hub sequence initialized", "room_id", h.roomID, "seq", seq)
}

func (h *Hub) registerSession(ctx context.Context, s *session, idleTimer *time.Timer) {
	if !idleTimer.Stop() {
		select {
		case <-idleTimer.C:
		default:
		}
	}

	if oldSession, ok := h.sessions[s.senderID]; ok {
		slog.InfoContext(ctx, "Session conflict, kicking old session", "sender_id", s.senderID, "room_id", h.roomID)
		sessionConflictsTotal.Add(ctx, 1)

		msg := &Message{
			Type:     msgTypeConflict,
			SenderID: systemSenderID,
			RoomID:   h.roomID,
			Content:  "disconnected due to multiple tabs in the same room",
		}
		rawData, err := msg.toRawJSON()
		if err != nil {
			slog.ErrorContext(ctx, "failed to marshal conflict message", "error", err)
		} else {
			oldSession.send(ctx, rawData)
		}

		oldSession.close()
		delete(h.sessions, s.senderID)
	}

	h.sessions[s.senderID] = s
	connectionsActive.Add(ctx, 1)
}

func (h *Hub) fanOut(ctx context.Context, message *Message) {
	seq := h.lastSequence.Add(1)
	message.SequenceNumber = seq

	if message.ID == "" {
		newID, err := uuid.NewV7()
		if err != nil {
			slog.ErrorContext(ctx, "failed to generate message ID", "error", err)
			return
		}
		message.ID = newID.String()
	}

	if message.Timestamp == 0 {
		message.Timestamp = time.Now().Unix()
	}

	rawData, err := message.toRawJSON()
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal fanout message", "error", err)
		return
	}
	for _, s := range h.sessions {
		s.sendWithMeta(ctx, rawData, message.SenderID, message.ReceivedAt)
	}

	if h.store != nil {
		msgCopy := *message
		persistChannelDepth.Record(ctx, float64(len(h.persistCh)))
		select {
		case h.persistCh <- &msgCopy:
		default:
			persistDroppedTotal.Add(ctx, 1)
			slog.WarnContext(ctx, "Persist channel full, message dropped",
				"room_id", h.roomID, "msg_id", message.ID, "seq", message.SequenceNumber)
		}
	}
}

func (h *Hub) shutdown() {
	for _, s := range h.sessions {
		s.close()
		connectionsActive.Add(context.Background(), -1)
	}
	h.sessions = make(map[string]*session)

	for {
		select {
		case req := <-h.registerCh:
			req.conn.Close()
			req.errCh <- errors.New("hub shutting down")
		case <-h.unregisterCh:
		case <-h.broadcastCh:
		default:
			return
		}
	}
}

func (h *Hub) register(ctx context.Context, conn *websocket.Conn, userID string) error {
	req := registerHubReq{
		conn:   conn,
		userID: userID,
		errCh:  make(chan error, 1),
	}
	select {
	case h.registerCh <- req:
		select {
		case err := <-req.errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-h.doneCh:
		return errors.New("hub closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hub) broadcast(ctx context.Context, msg *Message) {
	select {
	case h.broadcastCh <- msg:
	case <-h.doneCh:
		slog.InfoContext(ctx, "Hub closed during broadcast, dropped", "room_id", msg.RoomID)
	case <-ctx.Done():
	}
}

func (h *Hub) done() <-chan struct{} {
	return h.doneCh
}

func (h *Hub) forceClose() {
	h.stopOnce.Do(func() { close(h.stopCh) })
}
