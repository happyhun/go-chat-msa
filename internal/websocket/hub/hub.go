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
	persistDoneBufferSize   = 1
	handoffCloseCode        = 1012
	handoffCloseReason      = "room owner handoff"
	persistBackpressureMsg  = "message temporarily unavailable, please retry"
)

type registerHubReq struct {
	conn   *websocket.Conn
	userID string
	errCh  chan error
}

type persistTask struct {
	msg *Message
	ack func(error)
}

type MessageStore interface {
	GetLastSequenceNumber(ctx context.Context, roomID string) (int64, error)
	SaveMany(ctx context.Context, msgs []*Message) error
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
	persistCh    chan<- *persistTask
	persistSlots chan struct{}
	persistDone  chan struct{}

	doneCh   chan struct{}
	stopCh   chan struct{}
	stopOnce sync.Once

	acceptMu       sync.RWMutex
	drainCh        chan struct{}
	drainOnce      sync.Once
	drainTimeout   time.Duration
	draining       atomic.Bool
	pendingPersist atomic.Int64
	persistFailed  atomic.Bool
	activeSessions atomic.Int64
	closeReason    atomic.Value
}

func newHub(
	roomID string,
	sessionCfg sessionConfig,
	idleTimeout time.Duration,
	store MessageStore,
	persistCh chan<- *persistTask,
	drainTimeout time.Duration,
	allowFunc func(userID, roomID string) bool,
	persistSlots ...chan struct{},
) *Hub {
	cache, err := lru.New[string, *Message](idempotencyCacheSize)
	if err != nil {
		panic(fmt.Sprintf("hub: failed to create LRU cache: %v", err))
	}
	var slots chan struct{}
	if len(persistSlots) > 0 {
		slots = persistSlots[0]
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
		persistSlots:     slots,
		persistDone:      make(chan struct{}, persistDoneBufferSize),
		doneCh:           make(chan struct{}),
		stopCh:           make(chan struct{}),
		drainCh:          make(chan struct{}),
		drainTimeout:     drainTimeout,
	}
}

func (h *Hub) run(ctx context.Context) {
	sessionCtx, cancelSessions := context.WithCancel(ctx)

	slog.InfoContext(ctx, "Hub actor started", "room_id", h.roomID)
	defer func() {
		slog.InfoContext(ctx, "Hub actor stopped", "room_id", h.roomID)
		h.shutdown()
		close(h.doneCh)
		cancelSessions()
	}()

	h.initializeSequence(ctx)

	idleTimer := time.NewTimer(h.idleTimeout)
	idleTimer.Stop()

	for {
		select {
		case req := <-h.registerCh:
			if h.draining.Load() {
				if req.conn != nil {
					req.conn.Close()
				}
				req.errCh <- errors.New("hub shutting down")
				continue
			}
			s := newSession(h.sessionCfg, req.conn, req.userID, h.roomID, h.unregisterCh, h.enqueueBroadcast, h.allowFunc)
			h.registerSession(ctx, s, idleTimer)
			go s.run(sessionCtx)
			req.errCh <- nil

		case s := <-h.unregisterCh:
			if currentSession, ok := h.sessions[s.senderID]; ok && currentSession == s {
				delete(h.sessions, s.senderID)
				s.close()
				connectionsActive.Add(ctx, -1)
				h.activeSessions.Add(-1)
			}

			if len(h.sessions) == 0 {
				idleTimer.Reset(h.idleTimeout)
				slog.InfoContext(ctx, "Hub is empty, starting idle timer", "room_id", h.roomID)
			}

		case <-idleTimer.C:
			slog.InfoContext(ctx, "Hub idle timeout reached, shutting down", "room_id", h.roomID)
			hubsClosedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "idle")))
			h.beginDrain()
			h.drain(ctx)
			return

		case message := <-h.broadcastCh:
			h.handleBroadcast(ctx, message)

		case <-ctx.Done():
			h.beginDrain()
			h.drain(ctx)
			return

		case <-h.stopCh:
			slog.InfoContext(ctx, "Hub stopped by manager command", "room_id", h.roomID)
			hubsClosedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "force")))
			h.beginDrain()
			h.drain(ctx)
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
		h.activeSessions.Add(-1)
	}

	h.sessions[s.senderID] = s
	connectionsActive.Add(ctx, 1)
	h.activeSessions.Add(1)
}

func (h *Hub) fanOut(ctx context.Context, message *Message) {
	needsPersist := h.store != nil && h.persistCh != nil
	reservedPersistSlot := false
	if needsPersist {
		if !h.reservePersistSlot() {
			persistDroppedTotal.Add(ctx, 1)
			slog.WarnContext(ctx, "Persist channel full, message rejected",
				"room_id", h.roomID,
				"sender_id", message.SenderID,
				"reason", "persist_backpressure")
			h.sendSystemTo(ctx, message.SenderID, persistBackpressureMsg)
			return
		}
		reservedPersistSlot = true
	}

	seq := h.lastSequence.Add(1)
	message.SequenceNumber = seq

	if message.ID == "" {
		newID, err := uuid.NewV7()
		if err != nil {
			slog.ErrorContext(ctx, "failed to generate message ID", "error", err)
			h.rollbackSequenceAndSlot(reservedPersistSlot)
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
		h.rollbackSequenceAndSlot(reservedPersistSlot)
		return
	}

	if needsPersist {
		msgCopy := *message
		persistChannelDepth.Record(ctx, float64(len(h.persistCh)))
		h.pendingPersist.Add(1)
		task := &persistTask{
			msg: &msgCopy,
			ack: func(err error) {
				if err != nil {
					h.persistFailed.Store(true)
				}
				if h.pendingPersist.Add(-1) == 0 {
					h.notifyPersistDone()
				}
			},
		}
		select {
		case h.persistCh <- task:
		default:
			if h.pendingPersist.Add(-1) == 0 {
				h.notifyPersistDone()
			}
			h.rollbackSequenceAndSlot(reservedPersistSlot)
			persistDroppedTotal.Add(ctx, 1)
			slog.WarnContext(ctx, "Persist channel full, message rejected",
				"room_id", h.roomID, "msg_id", message.ID, "seq", message.SequenceNumber)
			h.sendSystemTo(ctx, message.SenderID, persistBackpressureMsg)
			return
		}
	}

	for _, s := range h.sessions {
		s.sendWithMeta(ctx, rawData, message.SenderID, message.ReceivedAt)
	}
}

func (h *Hub) handleBroadcast(ctx context.Context, message *Message) {
	broadcastChannelDepth.Record(ctx, float64(len(h.broadcastCh)))

	if message.ClientMsgID != "" {
		if _, exists := h.idempotencyCache.Get(message.ClientMsgID); exists {
			slog.InfoContext(ctx, "Duplicate message dropped (idempotency)", "client_msg_id", message.ClientMsgID, "room_id", h.roomID)
			duplicateMessagesDroppedTotal.Add(ctx, 1)
			return
		}
	}

	h.fanOut(ctx, message)

	if message.ClientMsgID != "" {
		h.idempotencyCache.Add(message.ClientMsgID, message)
	}
}

func (h *Hub) drain(ctx context.Context) {
	timeout := h.drainTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	start := time.Now()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		for {
			select {
			case <-ctx.Done():
				pending := h.pendingPersist.Load()
				slog.WarnContext(context.Background(), "Hub persist drain interrupted",
					"room_id", h.roomID,
					"pending_persist", pending,
					"broadcast_depth", len(h.broadcastCh),
					"error", ctx.Err())
				persistDrainTotal.Add(context.Background(), 1, metric.WithAttributes(attribute.String("status", "error")))
				persistDrainDuration.Record(context.Background(), time.Since(start).Seconds(), metric.WithAttributes(attribute.String("status", "error")))
				return
			case message := <-h.broadcastCh:
				h.handleBroadcast(ctx, message)
			default:
				goto drainedBroadcast
			}
		}

	drainedBroadcast:
		if len(h.broadcastCh) == 0 && h.pendingPersist.Load() == 0 {
			persistDrainTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
			persistDrainDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("status", "ok")))
			return
		}

		select {
		case message := <-h.broadcastCh:
			h.handleBroadcast(ctx, message)
		case <-h.persistDone:
		case <-ticker.C:
		case <-timer.C:
			pending := h.pendingPersist.Load()
			slog.WarnContext(ctx, "Hub persist drain timed out",
				"room_id", h.roomID,
				"pending_persist", pending,
				"broadcast_depth", len(h.broadcastCh),
				"timeout", timeout)
			persistDrainTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "timeout")))
			persistDrainDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("status", "timeout")))
			return
		case <-ctx.Done():
			pending := h.pendingPersist.Load()
			slog.WarnContext(context.Background(), "Hub persist drain interrupted",
				"room_id", h.roomID,
				"pending_persist", pending,
				"broadcast_depth", len(h.broadcastCh),
				"error", ctx.Err())
			persistDrainTotal.Add(context.Background(), 1, metric.WithAttributes(attribute.String("status", "error")))
			persistDrainDuration.Record(context.Background(), time.Since(start).Seconds(), metric.WithAttributes(attribute.String("status", "error")))
			return
		}
	}
}

func (h *Hub) notifyPersistDone() {
	select {
	case h.persistDone <- struct{}{}:
	default:
	}
}

func (h *Hub) reservePersistSlot() bool {
	if h.persistSlots == nil {
		return true
	}
	select {
	case h.persistSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *Hub) releasePersistSlot() {
	if h.persistSlots == nil {
		return
	}
	select {
	case <-h.persistSlots:
	default:
	}
}

func (h *Hub) rollbackSequenceAndSlot(reservedPersistSlot bool) {
	h.lastSequence.Add(-1)
	if reservedPersistSlot {
		h.releasePersistSlot()
	}
}

func (h *Hub) sendSystemTo(ctx context.Context, senderID, content string) {
	s, ok := h.sessions[senderID]
	if !ok {
		return
	}
	msg := &Message{
		Type:      msgTypeSystem,
		SenderID:  systemSenderID,
		RoomID:    h.roomID,
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

func (h *Hub) sessionCloseFrame() (string, int) {
	value := h.closeReason.Load()
	if value == nil {
		return "", 0
	}
	reason, _ := value.(string)
	switch reason {
	case handoffCloseReason, "lease_lost", "lease_renew_stale":
		return handoffCloseReason, handoffCloseCode
	default:
		return "", 0
	}
}

func (h *Hub) shutdown() {
	h.beginDrain()

	closeReason, closeCode := h.sessionCloseFrame()
	for _, s := range h.sessions {
		if closeCode != 0 {
			s.closeWithCode(closeCode, closeReason)
		} else {
			s.close()
		}
		connectionsActive.Add(context.Background(), -1)
	}
	h.activeSessions.Store(0)
	h.sessions = make(map[string]*session)

	for {
		select {
		case req := <-h.registerCh:
			if req.conn != nil {
				req.conn.Close()
			}
			req.errCh <- errors.New("hub shutting down")
		case <-h.unregisterCh:
		case <-h.broadcastCh:
		default:
			return
		}
	}
}

func (h *Hub) register(ctx context.Context, conn *websocket.Conn, userID string) error {
	if h.draining.Load() {
		return errors.New("hub shutting down")
	}

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
	if !h.enqueueBroadcast(ctx, msg) {
		slog.InfoContext(ctx, "Hub draining during broadcast, dropped", "room_id", msg.RoomID)
	}
}

func (h *Hub) enqueueBroadcast(ctx context.Context, msg *Message) bool {
	for {
		h.acceptMu.RLock()
		if h.draining.Load() {
			h.acceptMu.RUnlock()
			return false
		}
		select {
		case h.broadcastCh <- msg:
			h.acceptMu.RUnlock()
			return true
		default:
			h.acceptMu.RUnlock()
		}

		timer := time.NewTimer(time.Millisecond)
		select {
		case <-timer.C:
		case <-h.drainCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		case <-h.doneCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			slog.InfoContext(ctx, "Hub closed during broadcast, dropped", "room_id", msg.RoomID)
			return false
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		}
	}
}

func (h *Hub) done() <-chan struct{} {
	return h.doneCh
}

func (h *Hub) activeSessionCount() int64 {
	return h.activeSessions.Load()
}

func (h *Hub) isDraining() bool {
	return h.draining.Load()
}

func (h *Hub) forceClose(reasons ...string) {
	if len(reasons) > 0 && reasons[0] != "" {
		h.closeReason.Store(reasons[0])
	}
	h.beginDrain()
	h.stopOnce.Do(func() { close(h.stopCh) })
}

func (h *Hub) beginDrain() {
	h.drainOnce.Do(func() {
		h.acceptMu.Lock()
		defer h.acceptMu.Unlock()
		h.draining.Store(true)
		close(h.drainCh)
	})
}
