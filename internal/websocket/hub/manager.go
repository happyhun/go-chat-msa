package hub

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/ratelimit"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	hubDoneBufferSize     = 10
	defaultDrainTimeout   = 10 * time.Second
	maxSessionCloseJitter = 2 * time.Second
)

var (
	ErrRoomUnavailable = errors.New("room temporarily unavailable")
	ErrBusUnavailable  = errors.New("message bus unavailable")
	ErrManagerStopped  = errors.New("manager stopped")
)

type publishFunc func(context.Context, *Message) error

type prepareRegisterReq struct {
	ctx      context.Context
	roomID   string
	resultCh chan prepareRegisterResult
}

type prepareRegisterResult struct {
	registration *Registration
	err          error
}

type cancelPreparedReq struct {
	roomID string
	hub    *Hub
	doneCh chan struct{}
}

type closeRoomReq struct {
	roomID   string
	code     int
	reason   string
	resultCh chan bool
}

type closeAllReq struct {
	code   int
	reason string
	doneCh chan struct{}
}

type hubEntry struct {
	hub *Hub
	sub Subscription
}

type Manager struct {
	sessionCfg     sessionConfig
	idleTimeout    time.Duration
	drainTimeout   time.Duration
	maxDeliveryLag time.Duration
	selfPod        string
	bus            MessageBus
	limiter        *ratelimit.MemoryLimiter

	prepareRegisterCh  chan prepareRegisterReq
	cancelPreparedCh   chan cancelPreparedReq
	closeRoomCh        chan closeRoomReq
	closeAllCh         chan closeAllReq
	eventSub           Subscription
	busConnected       atomic.Bool
	connectionMu       sync.Mutex
	disconnectEpoch    uint64
	sessionsCloseEpoch uint64
	sessionCloseDelay  func() time.Duration
	stoppedCh          chan struct{}
	stoppedOnce        sync.Once
}

type Registration struct {
	manager *Manager
	roomID  string
	hub     *Hub
	created bool
	done    atomic.Bool
}

func NewManager(
	cfg config.ManagerConfig,
	rateCfg config.RateLimitConfig,
	bus MessageBus,
	selfPod string,
	maxDeliveryLag time.Duration,
	drainTimeout time.Duration,
) *Manager {
	if drainTimeout <= 0 {
		drainTimeout = defaultDrainTimeout
	}

	m := &Manager{
		sessionCfg: sessionConfig{
			writeWait:  cfg.WriteWait,
			pongWait:   cfg.PongWait,
			pingPeriod: cfg.PingPeriod,
			maxLength:  cfg.MaxLength,
		},
		idleTimeout:       cfg.IdleTimeout,
		drainTimeout:      drainTimeout,
		maxDeliveryLag:    maxDeliveryLag,
		selfPod:           selfPod,
		bus:               bus,
		limiter:           ratelimit.NewMemory(rateCfg.RPS, rateCfg.Burst, rateCfg.TTL),
		prepareRegisterCh: make(chan prepareRegisterReq),
		cancelPreparedCh:  make(chan cancelPreparedReq),
		closeRoomCh:       make(chan closeRoomReq),
		closeAllCh:        make(chan closeAllReq),
		sessionCloseDelay: func() time.Duration {
			// #nosec G404 -- Session-close jitter does not require cryptographic randomness.
			return time.Duration(rand.Int64N(int64(maxSessionCloseJitter)))
		},
		stoppedCh: make(chan struct{}),
	}
	m.busConnected.Store(bus == nil || bus.Connected())
	return m
}

func (m *Manager) Run(ctx context.Context) {
	hubs := make(map[string]*hubEntry)
	hubDoneCh := make(chan *Hub, hubDoneBufferSize)
	hubCtx, cancelHubs := context.WithCancel(context.Background())

	defer func() {
		cancelHubs()
		m.waitForHubsStopped(hubs)
		m.unsubscribeAll(hubs)
		m.limiter.Stop()
		m.stoppedOnce.Do(func() { close(m.stoppedCh) })
	}()

	m.subscribeEvents(ctx)

	createHub := func(reqCtx context.Context, roomID string) (*Hub, error) {
		h := newHub(roomID, m.sessionCfg, m.idleTimeout, m.allowMessage, m.publishMessage)

		sub, err := m.subscribeRoom(reqCtx, h)
		if err != nil {
			return nil, err
		}

		hubs[roomID] = &hubEntry{hub: h, sub: sub}
		hubsActive.Add(ctx, 1)
		go h.run(hubCtx)
		go func() {
			<-h.done()
			select {
			case hubDoneCh <- h:
			case <-ctx.Done():
			}
		}()
		return h, nil
	}

	getOrCreate := func(reqCtx context.Context, roomID string) (*Hub, bool, error) {
		if entry, ok := hubs[roomID]; ok {
			if entry.hub.isDraining() {
				return nil, false, ErrRoomUnavailable
			}
			return entry.hub, false, nil
		}
		h, err := createHub(reqCtx, roomID)
		if err != nil {
			return nil, false, err
		}
		return h, true, nil
	}

	slog.InfoContext(ctx, "Hub Manager started")

	defer slog.InfoContext(ctx, "Hub Manager stopped")

	for {
		select {
		case req := <-m.prepareRegisterCh:
			if !m.BusConnected() {
				req.resultCh <- prepareRegisterResult{err: ErrBusUnavailable}
				continue
			}
			h, created, err := getOrCreate(req.ctx, req.roomID)
			if err != nil {
				req.resultCh <- prepareRegisterResult{err: err}
				continue
			}
			req.resultCh <- prepareRegisterResult{
				registration: &Registration{
					manager: m,
					roomID:  req.roomID,
					hub:     h,
					created: created,
				},
			}

		case req := <-m.cancelPreparedCh:
			if entry, ok := hubs[req.roomID]; ok && entry.hub == req.hub && entry.hub.activeSessionCount() == 0 {
				entry.hub.forceClose(0, "")
			}
			close(req.doneCh)

		case req := <-m.closeRoomCh:
			entry, ok := hubs[req.roomID]
			if ok {
				entry.hub.forceClose(req.code, req.reason)
			}
			req.resultCh <- ok

		case req := <-m.closeAllCh:
			for _, entry := range hubs {
				entry.hub.forceClose(req.code, req.reason)
			}
			for roomID, entry := range hubs {
				<-entry.hub.done()
				m.unsubscribe(entry)
				delete(hubs, roomID)
				hubsActive.Add(ctx, -1)
			}
			close(req.doneCh)

		case h := <-hubDoneCh:
			if entry, ok := hubs[h.roomID]; ok && entry.hub == h {
				m.unsubscribe(entry)
				delete(hubs, h.roomID)
				hubsActive.Add(ctx, -1)
			}

		case <-ctx.Done():
			m.shutdownHubs(context.Background(), hubs)
			return
		}
	}
}

func (m *Manager) subscribeRoom(ctx context.Context, h *Hub) (Subscription, error) {
	if m.bus == nil {
		return nil, nil
	}

	roomID := h.roomID
	sub, err := m.bus.SubscribeRoom(ctx, roomID, func(env Envelope) {
		m.deliverToHub(h, env)
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to subscribe room", "room_id", roomID, "error", err)
		return nil, fmt.Errorf("%w: %w", ErrRoomUnavailable, err)
	}
	return sub, nil
}

func (m *Manager) deliverToHub(h *Hub, env Envelope) {
	ctx := context.Background()
	arrivedAt := time.Now()

	if !env.ReceivedAt.IsZero() {
		brokerHopDuration.Record(ctx, arrivedAt.Sub(env.ReceivedAt).Seconds())
	}

	if m.maxDeliveryLag > 0 && !env.ReceivedAt.IsZero() && arrivedAt.Sub(env.ReceivedAt) > m.maxDeliveryLag {
		slog.WarnContext(ctx, "delivery lag exceeded, closing room sessions",
			"room_id", env.RoomID, "lag_ms", arrivedAt.Sub(env.ReceivedAt).Milliseconds())
		m.closeRoomAsync(ctx, env.RoomID, closeCodeTryAgainLater, closeReasonDeliveryLag)
		return
	}

	h.deliver(ctx, deliverPacket{
		payload:    env.Payload,
		messageID:  env.MessageID,
		senderID:   env.SenderID,
		receivedAt: env.ReceivedAt,
		arrivedAt:  arrivedAt,
	})
}

func (m *Manager) subscribeEvents(ctx context.Context) {
	if m.bus == nil {
		return
	}

	sub, err := m.bus.SubscribeEvents(func(ev RoomEvent) {
		switch ev.Type {
		case EventRoomClosed:
			m.closeRoomAsync(context.Background(), ev.RoomID, closeCodeTryAgainLater, closeReasonRoomClosed)
		default:
			roomEventsIgnoredTotal.Add(context.Background(), 1,
				metric.WithAttributes(attribute.String("reason", "unknown_type")))
		}
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to subscribe room events", "error", err)
		return
	}
	m.eventSub = sub
}

func (m *Manager) unsubscribe(entry *hubEntry) {
	if entry == nil || entry.sub == nil {
		return
	}
	if err := entry.sub.Unsubscribe(); err != nil {
		slog.WarnContext(context.Background(), "failed to unsubscribe room", "room_id", entry.hub.roomID, "error", err)
	}
	entry.sub = nil
}

func (m *Manager) unsubscribeAll(hubs map[string]*hubEntry) {
	for _, entry := range hubs {
		m.unsubscribe(entry)
	}
	if m.eventSub != nil {
		if err := m.eventSub.Unsubscribe(); err != nil {
			slog.WarnContext(context.Background(), "failed to unsubscribe room events", "error", err)
		}
		m.eventSub = nil
	}
}

func (m *Manager) PrepareRegister(ctx context.Context, roomID string) (*Registration, error) {
	if !m.BusConnected() {
		return nil, ErrBusUnavailable
	}

	req := prepareRegisterReq{
		ctx:      ctx,
		roomID:   roomID,
		resultCh: make(chan prepareRegisterResult, 1),
	}
	select {
	case m.prepareRegisterCh <- req:
		select {
		case result := <-req.resultCh:
			return result.registration, result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-m.stoppedCh:
		return nil, ErrManagerStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Registration) Commit(ctx context.Context, conn *websocket.Conn, userID string) error {
	if r == nil || r.manager == nil || r.hub == nil {
		return errors.New("registration is not prepared")
	}
	if !r.manager.BusConnected() {
		return ErrBusUnavailable
	}
	if !r.done.CompareAndSwap(false, true) {
		return errors.New("registration already closed")
	}
	if err := r.hub.register(ctx, conn, userID); err != nil {
		if r.created {
			r.manager.cancelPrepared(context.Background(), r.roomID, r.hub)
		}
		return err
	}
	return nil
}

func (r *Registration) Cancel() {
	if r == nil || r.manager == nil || r.hub == nil {
		return
	}
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	if r.created {
		r.manager.cancelPrepared(context.Background(), r.roomID, r.hub)
	}
}

func (m *Manager) cancelPrepared(ctx context.Context, roomID string, h *Hub) {
	req := cancelPreparedReq{
		roomID: roomID,
		hub:    h,
		doneCh: make(chan struct{}),
	}
	select {
	case m.cancelPreparedCh <- req:
		select {
		case <-req.doneCh:
		case <-ctx.Done():
		}
	case <-m.stoppedCh:
	case <-ctx.Done():
	}
}

func (m *Manager) publishMessage(ctx context.Context, msg *Message) error {
	if m.Stopped() {
		return ErrManagerStopped
	}

	if msg.ID == "" {
		newID, idErr := uuid.NewV7()
		if idErr != nil {
			return fmt.Errorf("generate message id: %w", idErr)
		}
		msg.ID = newID.String()
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().Unix()
	}
	if msg.ReceivedAt.IsZero() {
		msg.ReceivedAt = time.Now()
	}

	payload, err := msg.toRawJSON()
	if err != nil {
		return err
	}

	if m.bus == nil {
		return nil
	}

	publishStarted := time.Now()
	err = m.bus.PublishMessage(ctx, Envelope{
		RoomID:     msg.RoomID,
		MessageID:  msg.ID,
		DedupID:    messageDedupID(msg),
		SenderID:   msg.SenderID,
		OriginPod:  m.selfPod,
		ReceivedAt: msg.ReceivedAt,
		Payload:    payload,
		Persistent: msg.Type == msgTypeChat,
	})
	if msg.Type == msgTypeChat {
		status := "accepted"
		if err != nil {
			status = "error"
		}
		jetStreamPublishDuration.Record(ctx, time.Since(publishStarted).Seconds(), metric.WithAttributes(attribute.String("status", status)))
	}
	if err != nil {
		natsPublishFailedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("subject_kind", "msg")))
		slog.WarnContext(ctx, "failed to publish message", "room_id", msg.RoomID, "error", err)
		return fmt.Errorf("%w: %w", ErrBusUnavailable, err)
	}

	return nil
}

func (m *Manager) allowMessage(userID, roomID string) bool {
	return m.limiter == nil || m.limiter.Allow(userID+":"+roomID)
}

func messageDedupID(msg *Message) string {
	sum := sha256.Sum256([]byte(msg.RoomID + "\x00" + msg.SenderID + "\x00" + msg.ClientMsgID + "\x00" + msg.Type + "\x00" + msg.Content))
	return fmt.Sprintf("%x", sum)
}

func (m *Manager) PublishSystemMessage(ctx context.Context, roomID, content string) error {
	msg, err := NewSystemMessage(roomID, content)
	if err != nil {
		return err
	}
	msg.ReceivedAt = time.Now()
	return m.publishMessage(ctx, msg)
}

func (m *Manager) CloseRoomSessions(ctx context.Context, roomID string) error {
	if m.bus == nil {
		return m.closeRoom(ctx, roomID, closeCodeTryAgainLater, closeReasonRoomClosed)
	}

	err := m.bus.PublishEvent(ctx, RoomEvent{
		Type:       EventRoomClosed,
		RoomID:     roomID,
		OccurredAt: time.Now(),
	})
	if err != nil {
		natsPublishFailedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("subject_kind", "event")))
		return fmt.Errorf("%w: %w", ErrBusUnavailable, err)
	}
	return nil
}

func (m *Manager) closeRoom(ctx context.Context, roomID string, code int, reason string) error {
	req := closeRoomReq{roomID: roomID, code: code, reason: reason, resultCh: make(chan bool, 1)}
	select {
	case m.closeRoomCh <- req:
		select {
		case <-req.resultCh:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-m.stoppedCh:
		return ErrManagerStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) closeRoomAsync(ctx context.Context, roomID string, code int, reason string) {
	go func() {
		if err := m.closeRoom(ctx, roomID, code, reason); err != nil && !errors.Is(err, ErrManagerStopped) {
			slog.WarnContext(ctx, "failed to close room sessions", "room_id", roomID, "reason", reason, "error", err)
		}
	}()
}

func (m *Manager) closeAllSessions(ctx context.Context, code int, reason string) {
	req := closeAllReq{code: code, reason: reason, doneCh: make(chan struct{})}
	select {
	case m.closeAllCh <- req:
		<-req.doneCh
	case <-m.stoppedCh:
	case <-ctx.Done():
	}
}

func (m *Manager) OnSlowConsumer(dropped int) {
	natsSlowConsumerTotal.Add(context.Background(), 1)
	if dropped > 0 {
		natsDroppedMessagesTotal.Add(context.Background(), int64(dropped))
	}
}

func (m *Manager) OnInvalidEvent() {
	roomEventsIgnoredTotal.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("reason", "invalid")))
}

func (m *Manager) OnDisconnected() {
	m.connectionMu.Lock()
	m.disconnectEpoch++
	disconnectEpoch := m.disconnectEpoch
	m.busConnected.Store(false)
	m.connectionMu.Unlock()
	natsDisconnectsTotal.Add(context.Background(), 1)
	slog.WarnContext(context.Background(), "nats disconnected, closing sessions")

	go func() {
		time.Sleep(m.sessionCloseDelay())
		m.connectionMu.Lock()
		current := m.disconnectEpoch == disconnectEpoch
		m.connectionMu.Unlock()
		if !current {
			return
		}
		m.closeAllSessions(context.Background(), closeCodeServiceRestart, closeReasonNATSDisconnect)
		m.connectionMu.Lock()
		defer m.connectionMu.Unlock()
		if m.disconnectEpoch != disconnectEpoch {
			return
		}
		m.sessionsCloseEpoch = disconnectEpoch
		if !m.Stopped() && (m.bus == nil || m.bus.Connected()) {
			m.busConnected.Store(true)
		}
	}()
}

func (m *Manager) OnReconnected() {
	m.connectionMu.Lock()
	defer m.connectionMu.Unlock()
	if m.sessionsCloseEpoch == m.disconnectEpoch && !m.Stopped() {
		m.busConnected.Store(true)
	}
	slog.InfoContext(context.Background(), "nats reconnected")
}

func (m *Manager) BusConnected() bool {
	return m.busConnected.Load() && (m.bus == nil || m.bus.Connected())
}

func (m *Manager) shutdownHubs(ctx context.Context, hubs map[string]*hubEntry) {
	if len(hubs) == 0 {
		return
	}

	for _, entry := range hubs {
		entry.hub.forceClose(closeCodeServiceRestart, closeReasonShutdown)
	}

	timeout := m.drainTimeout
	if timeout <= 0 {
		timeout = defaultDrainTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, entry := range hubs {
		select {
		case <-entry.hub.done():
		case <-waitCtx.Done():
			slog.WarnContext(ctx, "timed out waiting for hub shutdown", "room_id", entry.hub.roomID, "error", waitCtx.Err())
			return
		}
	}
}

func (m *Manager) waitForHubsStopped(hubs map[string]*hubEntry) {
	for _, entry := range hubs {
		<-entry.hub.done()
	}
}

func (m *Manager) Stopped() bool {
	select {
	case <-m.stoppedCh:
		return true
	default:
		return false
	}
}
