package hub

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/ratelimit"
	"go-chat-msa/internal/websocket/roomlease"

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
	maxRebalanceJitter       = 2 * time.Second
	leaseRenewInterval       = 10 * time.Second
	leaseMaxStale            = 20 * time.Second
	leaseReleaseRetryDelay   = time.Second
	defaultDrainTimeout      = 10 * time.Second
)

var (
	ErrRoomHandoffInProgress   = errors.New("room handoff in progress")
	ErrRoomSequenceUnavailable = errors.New("room sequence unavailable")
)

type OwnerRing interface {
	Locate(roomID string) string
}

type prepareRegisterReq struct {
	ctx      context.Context
	roomID   string
	resultCh chan prepareRegisterResult
}

type prepareRegisterResult struct {
	registration *Registration
	err          error
}

type registerReq struct {
	ctx    context.Context
	conn   *websocket.Conn
	userID string
	roomID string
	errCh  chan error
}

type cancelPreparedReq struct {
	roomID string
	hub    *Hub
	doneCh chan struct{}
}

type forceCloseReq struct {
	roomID   string
	reason   string
	resultCh chan bool
}

type persistRetryTask struct {
	tasks     []*persistTask
	attempts  int
	nextRetry time.Time
	createdAt time.Time
}

type managedLease struct {
	lease          *roomlease.Lease
	hub            *Hub
	acquiredAt     time.Time
	lastRenewOK    time.Time
	handoffAt      time.Time
	reason         string
	releaseWaiting bool
	releasing      bool
}

type hubLeaseState struct {
	lastSequence   int64
	pendingPersist int64
	broadcastDepth int
	activeSessions int64
	persistFailed  bool
}

type Manager struct {
	sessionCfg   sessionConfig
	idleTimeout  time.Duration
	drainTimeout time.Duration
	store        MessageStore
	floorStore   SequenceFloorStore
	limiter      *ratelimit.MemoryLimiter
	leaseStore   *roomlease.Store

	prepareRegisterCh chan prepareRegisterReq
	registerCh        chan registerReq
	cancelPreparedCh  chan cancelPreparedReq
	broadcastCh       chan *Message
	forceCloseCh      chan forceCloseReq
	persistCh         chan *persistTask
	persistSlots      chan struct{}
	retryCh           chan persistRetryTask
	listRoomsCh       chan chan []string

	workerWG    sync.WaitGroup
	retryWG     sync.WaitGroup
	leaseWG     sync.WaitGroup
	leasesMu    sync.RWMutex
	leases      map[string]*managedLease
	stoppedCh   chan struct{}
	stoppedOnce sync.Once
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
		idleTimeout:       cfg.IdleTimeout,
		drainTimeout:      drainTimeout,
		store:             store,
		prepareRegisterCh: make(chan prepareRegisterReq),
		registerCh:        make(chan registerReq),
		cancelPreparedCh:  make(chan cancelPreparedReq),
		broadcastCh:       make(chan *Message, broadcastBufferSize),
		forceCloseCh:      make(chan forceCloseReq),
		persistCh:         make(chan *persistTask, persistBufferSize),
		persistSlots:      make(chan struct{}, persistBufferSize),
		retryCh:           make(chan persistRetryTask, persistRetryQueueSize),
		listRoomsCh:       make(chan chan []string),
		leases:            make(map[string]*managedLease),
		stoppedCh:         make(chan struct{}),
		limiter:           limiter,
	}
}

func (m *Manager) SetRoomLeaseStore(store *roomlease.Store) {
	m.leaseStore = store
}

func (m *Manager) SetSequenceFloorStore(store SequenceFloorStore) {
	m.floorStore = store
}

func (m *Manager) PrepareRegister(ctx context.Context, roomID string) (*Registration, error) {
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
		return nil, errors.New("manager stopped")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Registration) Commit(ctx context.Context, conn *websocket.Conn, userID string) error {
	if r == nil || r.manager == nil || r.hub == nil {
		return errors.New("registration is not prepared")
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
		m.leaseWG.Wait()
		m.releaseAllLeases(context.Background(), hubs)
		m.stoppedOnce.Do(func() { close(m.stoppedCh) })
	}()

	createHub := func(reqCtx context.Context, roomID string) (*Hub, error) {
		allowFunc := func(userID, roomID string) bool {
			return m.limiter.Allow(userID + ":" + roomID)
		}

		store := m.store
		if store != nil && m.floorStore != nil {
			store = &sequenceFloorMessageStore{
				base:  store,
				floor: m.floorStore,
			}
		}

		h := newHub(roomID, m.sessionCfg, m.idleTimeout, store, m.persistCh, m.drainTimeout, allowFunc, m.persistSlots)
		if err := h.initializeSequence(reqCtx); err != nil {
			slog.ErrorContext(reqCtx, "failed to initialize room sequence",
				"room_id", roomID,
				"error", err)
			return nil, err
		}

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
		return h, nil
	}

	getOrCreate := func(reqCtx context.Context, roomID string) (*Hub, bool, error) {
		if h, ok := hubs[roomID]; ok {
			if h.isDraining() {
				return nil, false, ErrRoomHandoffInProgress
			}
			return h, false, nil
		}

		var lease *roomlease.Lease
		if m.leaseStore != nil {
			acquired, err := m.leaseStore.Acquire(reqCtx, roomID)
			if err != nil {
				return nil, false, err
			}
			lease = acquired
		}

		h, err := createHub(reqCtx, roomID)
		if err != nil {
			m.releaseAcquiredLease(reqCtx, lease)
			return nil, false, err
		}

		if lease != nil {
			m.trackLease(lease)
			slog.InfoContext(ctx, "room lease acquired",
				"room_id", roomID,
				"owner", lease.OwnerAddr)
		}

		return h, true, nil
	}

	slog.InfoContext(ctx, "Hub Manager started", "workers", workerPoolSize, "persist_buffer", persistBufferSize)

	for range workerPoolSize {
		m.workerWG.Add(1)
		go m.runPersistenceWorker()
	}
	m.retryWG.Add(1)
	go m.runPersistenceRetryWorker()

	if m.leaseStore != nil {
		m.leaseWG.Add(1)
		go m.runLeaseRenewer(ctx)
	}

	defer slog.InfoContext(ctx, "Hub Manager stopped")

	for {
		select {
		case req := <-m.prepareRegisterCh:
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

		case req := <-m.registerCh:
			h, _, err := getOrCreate(req.ctx, req.roomID)
			if err != nil {
				req.errCh <- err
				continue
			}
			go func() { req.errCh <- h.register(req.ctx, req.conn, req.userID) }()

		case req := <-m.cancelPreparedCh:
			if h, ok := hubs[req.roomID]; ok && h == req.hub && h.activeSessionCount() == 0 {
				h.forceClose("prepare_cancel")
			}
			close(req.doneCh)

		case msg := <-m.broadcastCh:
			h, _, err := getOrCreate(ctx, msg.RoomID)
			if err != nil {
				slog.WarnContext(ctx, "broadcast skipped: room lease unavailable",
					"room_id", msg.RoomID,
					"reason", err)
				continue
			}
			go h.broadcast(ctx, msg)

		case req := <-m.forceCloseCh:
			h, ok := hubs[req.roomID]
			if ok {
				if req.reason == handoffCloseReason || req.reason == "lease_lost" || req.reason == "lease_renew_stale" {
					m.markHandoff(req.roomID, req.reason)
				}
				h.forceClose(req.reason)
			}
			req.resultCh <- ok

		case h := <-hubDoneCh:
			if hubs[h.roomID] == h {
				delete(hubs, h.roomID)
				hubsActive.Add(ctx, -1)
				m.releaseLease(ctx, h.roomID, h)
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
			m.releasePersistSlot()
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
		if status.Code(err) == codes.Aborted {
			sequenceConflictTotal.Add(ctx, 1)
		}
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
			if status.Code(err) == codes.Aborted {
				sequenceConflictTotal.Add(ctx, 1)
			}
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
		h.forceClose(handoffCloseReason)
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

func (m *Manager) trackLease(lease *roomlease.Lease) {
	if lease == nil {
		return
	}
	now := time.Now()
	m.leasesMu.Lock()
	m.leases[lease.RoomID] = &managedLease{
		lease:       lease,
		acquiredAt:  now,
		lastRenewOK: now,
	}
	m.leasesMu.Unlock()
}

func (m *Manager) releaseAcquiredLease(ctx context.Context, lease *roomlease.Lease) {
	if lease == nil || m.leaseStore == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := m.leaseStore.Release(releaseCtx, lease); err != nil {
		slog.WarnContext(ctx, "failed to release room lease after hub creation failure",
			"room_id", lease.RoomID,
			"owner", lease.OwnerAddr,
			"error", err)
	}
}

func (m *Manager) markHandoff(roomID, reason string) {
	if reason == "" {
		reason = "unknown"
	}
	m.leasesMu.Lock()
	defer m.leasesMu.Unlock()
	managed, ok := m.leases[roomID]
	if !ok {
		return
	}
	if managed.handoffAt.IsZero() {
		managed.handoffAt = time.Now()
		managed.reason = reason
		roomHandoffTotal.Add(context.Background(), 1, metric.WithAttributes(attribute.String("status", "started")))
		slog.InfoContext(context.Background(), "room handoff started",
			"room_id", roomID,
			"owner", managed.lease.OwnerAddr,
			"reason", reason)
	}
}

func (m *Manager) releaseLease(ctx context.Context, roomID string, h *Hub) {
	m.leasesMu.RLock()
	managed, ok := m.leases[roomID]
	m.leasesMu.RUnlock()
	if !ok || managed.lease == nil || m.leaseStore == nil {
		return
	}
	if h == nil {
		h = managed.hub
	}
	if h != nil {
		m.rememberLeaseHub(roomID, managed, h)
	}

	state := captureHubLeaseState(h)
	if h != nil && !state.ready() {
		statusAttr := state.blockReason()
		slog.WarnContext(ctx, "room lease release deferred",
			"room_id", roomID,
			"owner", managed.lease.OwnerAddr,
			"reason", statusAttr,
			"last_sequence", state.lastSequence,
			"pending_persist", state.pendingPersist,
			"broadcast_depth", state.broadcastDepth,
			"active_sessions", state.activeSessions,
			"persist_failed", state.persistFailed)
		if state.persistFailed {
			if !m.releaseLeaseAfterPersistFailure(ctx, roomID, managed, state) {
				if m.markLeaseReleaseWaiting(roomID, managed, h) {
					go m.releaseLeaseWhenReady(roomID, managed, h)
				}
			}
			return
		}
		if m.markLeaseReleaseWaiting(roomID, managed, h) {
			go m.releaseLeaseWhenReady(roomID, managed, h)
		}
		return
	}

	if !m.releaseManagedLease(ctx, roomID, managed, state) && h != nil {
		if m.markLeaseReleaseWaiting(roomID, managed, h) {
			go m.releaseLeaseWhenReady(roomID, managed, h)
		}
	}
}

func (m *Manager) rememberLeaseHub(roomID string, managed *managedLease, h *Hub) {
	m.leasesMu.Lock()
	defer m.leasesMu.Unlock()
	if m.leases[roomID] == managed && managed.hub == nil {
		managed.hub = h
	}
}

func (m *Manager) markLeaseReleaseWaiting(roomID string, managed *managedLease, h *Hub) bool {
	m.leasesMu.Lock()
	defer m.leasesMu.Unlock()
	if m.leases[roomID] != managed {
		return false
	}
	if managed.releaseWaiting {
		return false
	}
	managed.releaseWaiting = true
	return true
}

func (m *Manager) releaseLeaseWhenReady(roomID string, managed *managedLease, h *Hub) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		state := captureHubLeaseState(h)
		if state.ready() {
			if m.releaseManagedLease(context.Background(), roomID, managed, state) {
				return
			}
			<-time.After(leaseReleaseRetryDelay)
			continue
		}
		if state.persistFailed {
			slog.WarnContext(context.Background(), "room lease release deferred aborted",
				"room_id", roomID,
				"owner", managed.lease.OwnerAddr,
				"reason", state.blockReason(),
				"last_sequence", state.lastSequence,
				"pending_persist", state.pendingPersist,
				"broadcast_depth", state.broadcastDepth,
				"active_sessions", state.activeSessions,
				"persist_failed", state.persistFailed)
			if m.releaseLeaseAfterPersistFailure(context.Background(), roomID, managed, state) {
				return
			}
			<-time.After(leaseReleaseRetryDelay)
			continue
		}

		if h == nil {
			<-ticker.C
			continue
		}
		select {
		case <-h.persistDone:
		case <-ticker.C:
		}
	}
}

func (m *Manager) releaseManagedLease(ctx context.Context, roomID string, managed *managedLease, state hubLeaseState) bool {
	m.leasesMu.Lock()
	if current := m.leases[roomID]; current != managed || managed.releasing {
		m.leasesMu.Unlock()
		return true
	}
	managed.releasing = true
	m.leasesMu.Unlock()

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()

	err := m.leaseStore.Release(releaseCtx, managed.lease)
	statusAttr := "success"
	if state.persistFailed {
		statusAttr = state.blockReason()
	}
	released := false
	if err != nil {
		statusAttr = "error"
		if errors.Is(err, roomlease.ErrLost) {
			statusAttr = "lost"
			released = true
			slog.WarnContext(ctx, "room lease release skipped",
				"room_id", roomID,
				"owner", managed.lease.OwnerAddr,
				"reason", "token_mismatch_or_missing")
		} else {
			slog.WarnContext(ctx, "room lease release failed",
				"room_id", roomID,
				"owner", managed.lease.OwnerAddr,
				"error", err)
		}
	} else {
		slog.InfoContext(ctx, "room lease released",
			"room_id", roomID,
			"owner", managed.lease.OwnerAddr,
			"last_sequence", state.lastSequence,
			"pending_persist", state.pendingPersist,
			"broadcast_depth", state.broadcastDepth,
			"active_sessions", state.activeSessions,
			"persist_failed", state.persistFailed)
		released = true
	}

	if released {
		m.leasesMu.Lock()
		if current := m.leases[roomID]; current == managed {
			delete(m.leases, roomID)
		}
		m.leasesMu.Unlock()
		m.recordHandoffResult(ctx, roomID, managed, statusAttr, state)
		return true
	}

	m.leasesMu.Lock()
	if current := m.leases[roomID]; current == managed {
		managed.releasing = false
	}
	m.leasesMu.Unlock()
	return false
}

func (m *Manager) releaseLeaseAfterPersistFailure(ctx context.Context, roomID string, managed *managedLease, state hubLeaseState) bool {
	if m.floorStore == nil {
		slog.ErrorContext(ctx, "room sequence floor store is unavailable",
			"room_id", roomID,
			"owner", managed.lease.OwnerAddr,
			"last_sequence", state.lastSequence,
			"persist_failed", state.persistFailed)
		return false
	}

	floorCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := m.floorStore.SetMax(floorCtx, roomID, state.lastSequence); err != nil {
		slog.ErrorContext(ctx, "failed to record room sequence floor",
			"room_id", roomID,
			"owner", managed.lease.OwnerAddr,
			"last_sequence", state.lastSequence,
			"error", err)
		return false
	}

	slog.WarnContext(ctx, "room sequence floor recorded after persist failure",
		"room_id", roomID,
		"owner", managed.lease.OwnerAddr,
		"last_sequence", state.lastSequence,
		"pending_persist", state.pendingPersist,
		"broadcast_depth", state.broadcastDepth,
		"active_sessions", state.activeSessions,
		"persist_failed", state.persistFailed)
	return m.releaseManagedLease(ctx, roomID, managed, state)
}

func captureHubLeaseState(h *Hub) hubLeaseState {
	if h == nil {
		return hubLeaseState{}
	}
	return hubLeaseState{
		lastSequence:   h.lastSequence.Load(),
		pendingPersist: h.pendingPersist.Load(),
		broadcastDepth: len(h.broadcastCh),
		activeSessions: h.activeSessionCount(),
		persistFailed:  h.persistFailed.Load(),
	}
}

func (s hubLeaseState) ready() bool {
	return s.pendingPersist == 0 && s.broadcastDepth == 0 && !s.persistFailed
}

func (s hubLeaseState) blockReason() string {
	if s.persistFailed {
		return "persist_failed"
	}
	return "drain_incomplete"
}

func (m *Manager) recordHandoffResult(ctx context.Context, roomID string, managed *managedLease, statusAttr string, state hubLeaseState) {
	if !managed.handoffAt.IsZero() {
		duration := time.Since(managed.handoffAt)
		roomHandoffTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", statusAttr)))
		roomHandoffDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attribute.String("status", statusAttr)))
		message := "room drain completed"
		logFn := slog.InfoContext
		if statusAttr != "success" {
			message = "room drain incomplete"
			logFn = slog.WarnContext
		}
		logFn(ctx, message,
			"room_id", roomID,
			"owner", managed.lease.OwnerAddr,
			"duration_ms", duration.Milliseconds(),
			"last_sequence", state.lastSequence,
			"pending_persist", state.pendingPersist,
			"broadcast_depth", state.broadcastDepth,
			"active_sessions", state.activeSessions,
			"persist_failed", state.persistFailed,
			"reason", managed.reason)
	}
}

func (m *Manager) releaseAllLeases(ctx context.Context, hubs map[string]*Hub) {
	m.leasesMu.RLock()
	roomIDs := make([]string, 0, len(m.leases))
	for roomID := range m.leases {
		roomIDs = append(roomIDs, roomID)
	}
	m.leasesMu.RUnlock()
	for _, roomID := range roomIDs {
		m.releaseLease(ctx, roomID, hubs[roomID])
	}
}

func (m *Manager) snapshotLeases() []*roomlease.Lease {
	m.leasesMu.RLock()
	defer m.leasesMu.RUnlock()
	leases := make([]*roomlease.Lease, 0, len(m.leases))
	for _, managed := range m.leases {
		leases = append(leases, managed.lease)
	}
	return leases
}

func (m *Manager) updateLeaseRenewOK(roomID string, at time.Time) {
	m.leasesMu.Lock()
	if managed, ok := m.leases[roomID]; ok {
		managed.lastRenewOK = at
	}
	m.leasesMu.Unlock()
}

func (m *Manager) leaseLastRenewOK(roomID string) time.Time {
	m.leasesMu.RLock()
	defer m.leasesMu.RUnlock()
	if managed, ok := m.leases[roomID]; ok {
		return managed.lastRenewOK
	}
	return time.Time{}
}

func (m *Manager) runLeaseRenewer(ctx context.Context) {
	defer m.leaseWG.Done()

	ticker := time.NewTicker(leaseRenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.renewLeases(ctx)
		}
	}
}

func (m *Manager) renewLeases(ctx context.Context) {
	leases := m.snapshotLeases()
	if len(leases) == 0 || m.leaseStore == nil {
		return
	}

	results := m.leaseStore.RenewBatch(ctx, leases)
	now := time.Now()
	for _, result := range results {
		if result.Lease == nil {
			continue
		}
		roomID := result.Lease.RoomID
		if result.Err == nil {
			m.updateLeaseRenewOK(roomID, now)
			continue
		}
		if errors.Is(result.Err, roomlease.ErrLost) {
			slog.ErrorContext(ctx, "room lease lost",
				"room_id", roomID,
				"owner", result.Lease.OwnerAddr,
				"reason", "token_mismatch_or_missing")
			_, _ = m.forceCloseRoom(ctx, roomID, "lease_lost")
			continue
		}
		lastOK := m.leaseLastRenewOK(roomID)
		if lastOK.IsZero() || now.Sub(lastOK) > leaseMaxStale {
			slog.WarnContext(ctx, "room lease renew stale, draining room",
				"room_id", roomID,
				"owner", result.Lease.OwnerAddr,
				"duration_ms", now.Sub(lastOK).Milliseconds(),
				"error", result.Err)
			_, _ = m.forceCloseRoom(ctx, roomID, "lease_renew_stale")
			continue
		}
		slog.WarnContext(ctx, "room lease renew failed",
			"room_id", roomID,
			"owner", result.Lease.OwnerAddr,
			"duration_ms", now.Sub(lastOK).Milliseconds(),
			"error", result.Err)
	}
}

func (m *Manager) PersistQueueUtilization() float64 {
	if cap(m.persistCh) == 0 {
		return 0
	}
	return float64(len(m.persistCh)) / float64(cap(m.persistCh))
}

func (m *Manager) releasePersistSlot() {
	if m.persistSlots == nil {
		return
	}
	select {
	case <-m.persistSlots:
	default:
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
		closed, err := m.forceCloseRoom(ctx, room, "room owner handoff")
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
	return m.forceCloseRoom(ctx, roomID, "force_close")
}

func (m *Manager) forceCloseRoom(ctx context.Context, roomID, reason string) (bool, error) {
	if reason == "" {
		reason = "force_close"
	}
	req := forceCloseReq{roomID: roomID, reason: reason, resultCh: make(chan bool, 1)}
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
