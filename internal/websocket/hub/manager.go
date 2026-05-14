package hub

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/ratelimit"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	broadcastBufferSize      = 500
	hubDoneBufferSize        = 10
	workerPoolSize           = 4
	persistBufferSize        = 10000
	persistBatchSize         = 500
	persistFlushTimeout      = 100 * time.Millisecond
	persistRetryQueueSize    = 250
	persistRetryMaxAttempts  = 5
	persistRetryInitBackoff  = 1 * time.Second
	persistRetryMaxBackoff   = 30 * time.Second
	persistRetryTickInterval = 500 * time.Millisecond
	ownerCheckInterval       = 10 * time.Second
	maxRebalanceJitter       = 10 * time.Second
	defaultDrainTimeout      = 10 * time.Second
)

type OwnerRing interface {
	Locate(roomID string) string
}

type registerReq struct {
	ctx    context.Context
	conn   *websocket.Conn
	userID string
	roomID string
	errCh  chan error
}

type forceCloseReq struct {
	roomID   string
	resultCh chan bool
}

type persistRetryTask struct {
	tasks     []*persistTask
	attempts  int
	nextRetry time.Time
	createdAt time.Time
}

type Manager struct {
	sessionCfg   sessionConfig
	idleTimeout  time.Duration
	drainTimeout time.Duration
	store        MessageStore
	limiter      *ratelimit.MemoryLimiter

	registerCh   chan registerReq
	broadcastCh  chan *Message
	forceCloseCh chan forceCloseReq
	persistCh    chan *persistTask
	retryCh      chan persistRetryTask
	listRoomsCh  chan chan []string

	workerWG    sync.WaitGroup
	retryWG     sync.WaitGroup
	stoppedCh   chan struct{}
	stoppedOnce sync.Once
}

func NewManager(
	cfg config.ManagerConfig,
	rateCfg config.RateLimitConfig,
	store MessageStore,
	drainTimeouts ...time.Duration,
) *Manager {
	limiter := ratelimit.NewMemory(
		rateCfg.RPS,
		rateCfg.Burst,
		rateCfg.TTL,
	)
	drainTimeout := defaultDrainTimeout
	if len(drainTimeouts) > 0 && drainTimeouts[0] > 0 {
		drainTimeout = drainTimeouts[0]
	}

	return &Manager{
		sessionCfg: sessionConfig{
			writeWait:  cfg.WriteWait,
			pongWait:   cfg.PongWait,
			pingPeriod: cfg.PingPeriod,
			maxLength:  cfg.MaxLength,
		},
		idleTimeout:  cfg.IdleTimeout,
		drainTimeout: drainTimeout,
		store:        store,
		registerCh:   make(chan registerReq),
		broadcastCh:  make(chan *Message, broadcastBufferSize),
		forceCloseCh: make(chan forceCloseReq),
		persistCh:    make(chan *persistTask, persistBufferSize),
		retryCh:      make(chan persistRetryTask, persistRetryQueueSize),
		listRoomsCh:  make(chan chan []string),
		stoppedCh:    make(chan struct{}),
		limiter:      limiter,
	}
}

func (m *Manager) Run(ctx context.Context) {
	hubs := make(map[string]*Hub)
	hubDoneCh := make(chan *Hub, hubDoneBufferSize)
	hubCtx, cancelHubs := context.WithCancel(context.Background())

	defer func() {
		cancelHubs()
		m.waitForHubsStopped(hubs)
		m.limiter.Stop()
		close(m.persistCh)
		m.workerWG.Wait()
		close(m.retryCh)
		m.retryWG.Wait()
		m.stoppedOnce.Do(func() { close(m.stoppedCh) })
	}()

	getOrCreate := func(roomID string) *Hub {
		if h, ok := hubs[roomID]; ok {
			return h
		}

		allowFunc := func(userID, roomID string) bool {
			return m.limiter.Allow(userID + ":" + roomID)
		}

		h := newHub(roomID, m.sessionCfg, m.idleTimeout, m.store, m.persistCh, m.drainTimeout, allowFunc)
		hubs[roomID] = h
		hubsActive.Add(ctx, 1)
		go h.run(hubCtx)
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
	m.retryWG.Add(1)
	go m.runPersistenceRetryWorker()

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
				h.forceClose()
			}
			req.resultCh <- ok

		case h := <-hubDoneCh:
			if hubs[h.roomID] == h {
				delete(hubs, h.roomID)
				hubsActive.Add(ctx, -1)
			}

		case respCh := <-m.listRoomsCh:
			respCh <- slices.Collect(maps.Keys(hubs))

		case <-ctx.Done():
			m.shutdownHubs(context.Background(), hubs)
			return
		}
	}
}

func (m *Manager) runPersistenceWorker() {
	defer m.workerWG.Done()

	batch := make([]*persistTask, 0, persistBatchSize)
	ticker := time.NewTicker(persistFlushTimeout)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		m.savePersistTasks(context.Background(), batch)
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

func (m *Manager) savePersistTasks(ctx context.Context, tasks []*persistTask) {
	if len(tasks) == 0 {
		return
	}
	if m.store == nil {
		ackPersistTasks(tasks, nil)
		return
	}

	msgs := messagesFromPersistTasks(tasks)
	err := m.store.SaveMany(ctx, msgs)
	if err == nil || isIdempotentPersistSuccess(err) {
		persistenceBatchSaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
		ackPersistTasks(tasks, nil)
		return
	}
	if !isRetryablePersistError(err) {
		persistenceBatchSaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "reject")))
		slog.ErrorContext(ctx, "batch save permanently failed (non-retryable)",
			"count", len(tasks), "error", err)
		ackPersistTasks(tasks, err)
		return
	}

	m.enqueuePersistRetry(ctx, tasks, err)
}

func (m *Manager) enqueuePersistRetry(ctx context.Context, tasks []*persistTask, cause error) {
	now := time.Now()
	retryTask := persistRetryTask{
		tasks:     append([]*persistTask(nil), tasks...),
		attempts:  1,
		nextRetry: now.Add(jitteredPersistBackoff(1)),
		createdAt: now,
	}
	select {
	case m.retryCh <- retryTask:
		persistenceRetryQueueDepth.Record(ctx, float64(len(m.retryCh)))
		persistenceBatchSaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "retry")))
		slog.WarnContext(ctx, "batch enqueued for retry", "count", len(tasks), "error", cause)
	default:
		persistenceRetryQueueFullTotal.Add(ctx, 1)
		slog.ErrorContext(ctx, "retry queue full, batch dropped", "count", len(tasks), "error", cause)
		ackPersistTasks(tasks, cause)
	}
}

func (m *Manager) runPersistenceRetryWorker() {
	defer m.retryWG.Done()

	var batch []persistRetryTask
	ticker := time.NewTicker(persistRetryTickInterval)
	defer ticker.Stop()

	for {
		select {
		case task, ok := <-m.retryCh:
			if !ok {
				m.drainRetryBatch(context.Background(), batch)
				return
			}
			batch = append(batch, task)
			persistenceRetryQueueDepth.Record(context.Background(), float64(len(m.retryCh)))
		case <-ticker.C:
			if len(batch) == 0 {
				continue
			}
			updatePersistRetryMetrics(batch)
			batch = m.processRetryBatch(context.Background(), batch)
		}
	}
}

func (m *Manager) processRetryBatch(ctx context.Context, batch []persistRetryTask) []persistRetryTask {
	now := time.Now()
	remaining := batch[:0]

	for _, task := range batch {
		if now.Before(task.nextRetry) {
			remaining = append(remaining, task)
			continue
		}

		err := m.store.SaveMany(ctx, messagesFromPersistTasks(task.tasks))
		if err == nil || isIdempotentPersistSuccess(err) {
			persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
			slog.InfoContext(ctx, "batch retry succeeded",
				"count", len(task.tasks), "attempts", task.attempts)
			ackPersistTasks(task.tasks, nil)
			continue
		}

		if !isRetryablePersistError(err) {
			persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "reject")))
			slog.ErrorContext(ctx, "batch retry permanently failed (non-retryable)",
				"error", err, "count", len(task.tasks))
			ackPersistTasks(task.tasks, err)
			continue
		}

		task.attempts++
		if task.attempts > persistRetryMaxAttempts {
			persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "exhaust")))
			slog.ErrorContext(ctx, "batch exhausted all retries",
				"count", len(task.tasks), "attempts", task.attempts-1, "error", err)
			ackPersistTasks(task.tasks, err)
			continue
		}

		persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "retry")))
		task.nextRetry = now.Add(jitteredPersistBackoff(task.attempts))
		remaining = append(remaining, task)
	}

	return remaining
}

func (m *Manager) drainRetryBatch(ctx context.Context, batch []persistRetryTask) {
drain:
	for {
		select {
		case task, ok := <-m.retryCh:
			if !ok {
				break drain
			}
			batch = append(batch, task)
		default:
			break drain
		}
	}

	if len(batch) == 0 {
		return
	}

	slog.InfoContext(ctx, "draining retry queue on shutdown", "count", len(batch))
	for _, task := range batch {
		err := m.store.SaveMany(ctx, messagesFromPersistTasks(task.tasks))
		if err != nil && !isIdempotentPersistSuccess(err) {
			slog.ErrorContext(ctx, "failed to save batch during shutdown drain",
				"error", err, "count", len(task.tasks))
			ackPersistTasks(task.tasks, err)
			continue
		}
		ackPersistTasks(task.tasks, nil)
	}
}

func (m *Manager) shutdownHubs(ctx context.Context, hubs map[string]*Hub) {
	if len(hubs) == 0 {
		return
	}

	for _, h := range hubs {
		h.forceClose()
	}

	timeout := m.drainTimeout
	if timeout <= 0 {
		timeout = defaultDrainTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, h := range hubs {
		select {
		case <-h.done():
		case <-waitCtx.Done():
			slog.WarnContext(ctx, "timed out waiting for hub shutdown", "room_id", h.roomID, "error", waitCtx.Err())
			return
		}
	}
}

func (m *Manager) waitForHubsStopped(hubs map[string]*Hub) {
	for _, h := range hubs {
		<-h.done()
	}
}

func (m *Manager) PersistQueueUtilization() float64 {
	if cap(m.persistCh) == 0 {
		return 0
	}
	return float64(len(m.persistCh)) / float64(cap(m.persistCh))
}

func (m *Manager) Stopped() bool {
	select {
	case <-m.stoppedCh:
		return true
	default:
		return false
	}
}

func messagesFromPersistTasks(tasks []*persistTask) []*Message {
	msgs := make([]*Message, 0, len(tasks))
	for _, task := range tasks {
		msgs = append(msgs, task.msg)
	}
	return msgs
}

func ackPersistTasks(tasks []*persistTask, err error) {
	for _, task := range tasks {
		if task.ack != nil {
			task.ack(err)
		}
	}
}

func updatePersistRetryMetrics(batch []persistRetryTask) {
	if len(batch) == 0 {
		persistenceRetryOldestAge.Record(context.Background(), 0)
		return
	}
	oldest := batch[0].createdAt
	for _, t := range batch[1:] {
		if t.createdAt.Before(oldest) {
			oldest = t.createdAt
		}
	}
	persistenceRetryOldestAge.Record(context.Background(), time.Since(oldest).Seconds())
}

func jitteredPersistBackoff(attempts int) time.Duration {
	base := min(persistRetryInitBackoff*(1<<min(attempts-1, 5)), persistRetryMaxBackoff)
	return time.Duration(rand.Int64N(int64(base)))
}

func isIdempotentPersistSuccess(err error) bool {
	return status.Code(err) == codes.AlreadyExists
}

func isRetryablePersistError(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return true
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.ResourceExhausted:
		return true
	default:
		return false
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

func (m *Manager) WatchOwnership(ctx context.Context, ring OwnerRing, addr string, events <-chan struct{}) {
	ticker := time.NewTicker(ownerCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-events:
		}
		m.reconcileOwnership(ctx, ring, addr)
	}
}

func (m *Manager) reconcileOwnership(ctx context.Context, ring OwnerRing, addr string) {
	respCh := make(chan []string, 1)
	select {
	case m.listRoomsCh <- respCh:
	case <-m.stoppedCh:
		return
	case <-ctx.Done():
		return
	}

	rooms := <-respCh
	for _, room := range rooms {
		if ring.Locate(room) != addr {
			m.scheduleRebalanceClose(ctx, room)
		}
	}
}

func (m *Manager) scheduleRebalanceClose(ctx context.Context, room string) {
	jitter := time.Duration(rand.Int64N(int64(maxRebalanceJitter)))
	time.AfterFunc(jitter, func() {
		if ctx.Err() != nil {
			return
		}
		closed, err := m.ForceCloseRoom(ctx, room)
		if err != nil {
			slog.WarnContext(ctx, "rebalance close failed", "room_id", room, "error", err)
			return
		}
		if !closed {
			return
		}
		slog.InfoContext(ctx, "rebalance close fired", "room_id", room, "jitter_ms", jitter.Milliseconds())
		rebalanceEvictionsTotal.Add(ctx, 1)
	})
}

func (m *Manager) ForceCloseRoom(ctx context.Context, roomID string) (bool, error) {
	req := forceCloseReq{roomID: roomID, resultCh: make(chan bool, 1)}
	select {
	case m.forceCloseCh <- req:
		select {
		case closed := <-req.resultCh:
			return closed, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	case <-m.stoppedCh:
		return false, errors.New("manager stopped")
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
