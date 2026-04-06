package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/ratelimit"

	"github.com/gorilla/websocket"
)

const (
	broadcastBufferSize = 500
	hubDoneBufferSize   = 10
	workerPoolSize      = 4
	persistBufferSize   = 10000
	persistBatchSize    = 500
	persistFlushTimeout = 100 * time.Millisecond
)

type registerReq struct {
	ctx    context.Context
	conn   *websocket.Conn
	userID string
	roomID string
	errCh  chan error
}

type forceCloseReq struct {
	roomID string
	doneCh chan struct{}
}

type Manager struct {
	sessionCfg  sessionConfig
	idleTimeout time.Duration
	store       MessageStore
	limiter     *ratelimit.Limiter

	registerCh   chan registerReq
	broadcastCh  chan *Message
	forceCloseCh chan forceCloseReq
	persistCh    chan *Message

	workerWG    sync.WaitGroup
	stoppedCh   chan struct{}
	stoppedOnce sync.Once
}

func NewManager(
	cfg config.ManagerConfig,
	rateCfg config.RateLimitConfig,
	store MessageStore,
) *Manager {
	limiter := ratelimit.New(
		rateCfg.RPS,
		rateCfg.Burst,
		rateCfg.TTL,
	)

	return &Manager{
		sessionCfg: sessionConfig{
			writeWait:  cfg.WriteWait,
			pongWait:   cfg.PongWait,
			pingPeriod: cfg.PingPeriod,
			maxLength:  cfg.MaxLength,
		},
		idleTimeout:  cfg.IdleTimeout,
		store:        store,
		registerCh:   make(chan registerReq),
		broadcastCh:  make(chan *Message, broadcastBufferSize),
		forceCloseCh: make(chan forceCloseReq),
		persistCh:    make(chan *Message, persistBufferSize),
		stoppedCh:    make(chan struct{}),
		limiter:      limiter,
	}
}

func (m *Manager) Run(ctx context.Context) {
	hubs := make(map[string]*Hub)
	hubDoneCh := make(chan *Hub, hubDoneBufferSize)

	defer func() {
		m.limiter.Stop()
		close(m.persistCh)
		m.workerWG.Wait()
		m.stoppedOnce.Do(func() { close(m.stoppedCh) })
	}()

	getOrCreate := func(roomID string) *Hub {
		if h, ok := hubs[roomID]; ok {
			return h
		}

		allowFunc := func(userID, roomID string) bool {
			return m.limiter.Allow(userID + ":" + roomID)
		}

		h := newHub(roomID, m.sessionCfg, m.idleTimeout, m.store, m.persistCh, allowFunc)
		hubs[roomID] = h
		hubsActive.Add(ctx, 1)
		go h.run(ctx)
		go func() {
			<-h.done()
			select {
			case hubDoneCh <- h:
			case <-ctx.Done():
			}
		}()
		return h
	}

	slog.InfoContext(ctx, "Hub Manager started", "workers", workerPoolSize, "persist_buffer", persistBufferSize)

	for range workerPoolSize {
		m.workerWG.Add(1)
		go m.runPersistenceWorker()
	}

	defer slog.InfoContext(ctx, "Hub Manager stopped")

	for {
		select {
		case req := <-m.registerCh:
			h := getOrCreate(req.roomID)
			go func() { req.errCh <- h.register(req.ctx, req.conn, req.userID) }()

		case msg := <-m.broadcastCh:
			h := getOrCreate(msg.RoomID)
			go h.broadcast(ctx, msg)

		case req := <-m.forceCloseCh:
			h, ok := hubs[req.roomID]
			if ok {
				delete(hubs, req.roomID)
				hubsActive.Add(ctx, -1)
				h.forceClose()
			}
			close(req.doneCh)

		case h := <-hubDoneCh:
			if hubs[h.roomID] == h {
				delete(hubs, h.roomID)
				hubsActive.Add(ctx, -1)
			}

		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) runPersistenceWorker() {
	defer m.workerWG.Done()

	batch := make([]*Message, 0, persistBatchSize)
	ticker := time.NewTicker(persistFlushTimeout)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		m.store.SaveMany(context.Background(), batch)
		batch = batch[:0]
	}

	for {
		select {
		case msg, ok := <-m.persistCh:
			if !ok {
				flush()
				return
			}
			persistChannelDepth.Record(context.Background(), float64(len(m.persistCh)))
			batch = append(batch, msg)
			if len(batch) >= persistBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (m *Manager) Register(ctx context.Context, conn *websocket.Conn, userID, roomID string) error {
	req := registerReq{
		ctx:    ctx,
		conn:   conn,
		userID: userID,
		roomID: roomID,
		errCh:  make(chan error, 1),
	}
	select {
	case m.registerCh <- req:
		select {
		case err := <-req.errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-m.stoppedCh:
		return errors.New("manager stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Broadcast(ctx context.Context, msg *Message) error {
	select {
	case m.broadcastCh <- msg:
		return nil
	case <-m.stoppedCh:
		return errors.New("manager stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) ForceCloseRoom(ctx context.Context, roomID string) error {
	req := forceCloseReq{roomID: roomID, doneCh: make(chan struct{})}
	select {
	case m.forceCloseCh <- req:
		select {
		case <-req.doneCh:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-m.stoppedCh:
		return errors.New("manager stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}
