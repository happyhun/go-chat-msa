package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	systemSenderID = "system"

	hubUnregisterBufferSize = 50
	hubBroadcastBufferSize  = 250

	closeCodeServiceRestart = 1012
	closeCodeTryAgainLater  = 1013

	closeReasonRoomClosed     = "room_closed"
	closeReasonShutdown       = "shutdown"
	closeReasonSlowConsumer   = "slow_consumer"
	closeReasonDeliveryLag    = "delivery_lag"
	closeReasonNATSDisconnect = "nats_disconnected"
)

type closeSignal struct {
	code   int
	reason string
}

type registerHubReq struct {
	conn   *websocket.Conn
	userID string
	errCh  chan error
}

type deliverPacket struct {
	payload    []byte
	messageID  string
	senderID   string
	receivedAt time.Time
	arrivedAt  time.Time
}

type Hub struct {
	roomID      string
	sessionCfg  sessionConfig
	idleTimeout time.Duration
	allowFunc   func(userID, roomID string) bool
	publish     publishFunc

	sessions      map[string]*session
	lastDelivered string

	registerCh   chan registerHubReq
	unregisterCh chan *session
	broadcastCh  chan deliverPacket

	doneCh   chan struct{}
	stopCh   chan struct{}
	stopOnce sync.Once

	draining       atomic.Bool
	activeSessions atomic.Int64
	closing        atomic.Pointer[closeSignal]
}

func newHub(
	roomID string,
	sessionCfg sessionConfig,
	idleTimeout time.Duration,
	allowFunc func(userID, roomID string) bool,
	publish publishFunc,
) *Hub {
	return &Hub{
		roomID:       roomID,
		sessionCfg:   sessionCfg,
		idleTimeout:  idleTimeout,
		allowFunc:    allowFunc,
		publish:      publish,
		sessions:     make(map[string]*session),
		registerCh:   make(chan registerHubReq),
		unregisterCh: make(chan *session, hubUnregisterBufferSize),
		broadcastCh:  make(chan deliverPacket, hubBroadcastBufferSize),
		doneCh:       make(chan struct{}),
		stopCh:       make(chan struct{}),
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

	idleTimer := time.NewTimer(h.idleTimeout)
	idleTimer.Stop()

	for {
		select {
		case req := <-h.registerCh:
			if h.draining.Load() {
				if req.conn != nil {
					_ = req.conn.Close()
				}
				req.errCh <- errors.New("hub shutting down")
				continue
			}
			s := newSession(uuid.NewString(), h.sessionCfg, req.conn, req.userID, h.roomID,
				h.unregisterCh, h.publish, h.allowFunc)
			h.registerSession(ctx, s, idleTimer)
			go s.run(sessionCtx)
			req.errCh <- nil

		case s := <-h.unregisterCh:
			if current, ok := h.sessions[s.id]; ok && current == s {
				delete(h.sessions, s.id)
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
			h.draining.Store(true)
			return

		case packet := <-h.broadcastCh:
			h.fanOut(ctx, packet)

		case <-ctx.Done():
			h.draining.Store(true)
			return

		case <-h.stopCh:
			slog.InfoContext(ctx, "Hub stopped by manager command", "room_id", h.roomID)
			hubsClosedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "force")))
			h.draining.Store(true)
			return
		}
	}
}

func (h *Hub) registerSession(ctx context.Context, s *session, idleTimer *time.Timer) {
	if !idleTimer.Stop() {
		select {
		case <-idleTimer.C:
		default:
		}
	}

	h.sessions[s.id] = s
	connectionsActive.Add(ctx, 1)
	h.activeSessions.Add(1)
}

func (h *Hub) fanOut(ctx context.Context, packet deliverPacket) {
	broadcastChannelDepth.Record(ctx, float64(len(h.broadcastCh)))
	h.observeOrder(ctx, packet.messageID)

	for _, s := range h.sessions {
		s.send(ctx, egressPacket{
			data:       packet.payload,
			senderID:   packet.senderID,
			receivedAt: packet.receivedAt,
		})
	}

	if !packet.arrivedAt.IsZero() {
		hubFanoutDuration.Record(ctx, time.Since(packet.arrivedAt).Seconds())
	}
}

func (h *Hub) observeOrder(ctx context.Context, messageID string) {
	if messageID == "" {
		return
	}

	if h.lastDelivered != "" && messageID < h.lastDelivered {
		outOfOrderTotal.Add(ctx, 1)
		if span, ok := uuidV7Gap(h.lastDelivered, messageID); ok {
			reorderSpanSeconds.Record(ctx, span.Seconds())
		}
		return
	}
	h.lastDelivered = messageID
}

func (h *Hub) shutdown() {
	h.draining.Store(true)

	signal := h.closing.Load()
	for _, s := range h.sessions {
		if signal != nil {
			s.closeWithCode(signal.code, signal.reason)
			sessionsClosedTotal.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", signal.reason)))
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
				_ = req.conn.Close()
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

func (h *Hub) deliver(ctx context.Context, packet deliverPacket) {
	if h.draining.Load() {
		return
	}
	select {
	case h.broadcastCh <- packet:
	case <-h.doneCh:
	case <-ctx.Done():
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

func (h *Hub) forceClose(code int, reason string) {
	if reason != "" {
		h.closing.Store(&closeSignal{code: code, reason: reason})
	}
	h.draining.Store(true)
	h.stopOnce.Do(func() { close(h.stopCh) })
}
