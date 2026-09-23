package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go-chat-msa/internal/shared/config"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"
)

const (
	persistenceStream    = "CHAT_PERSIST"
	persistenceSubject   = "chat.persist.*"
	persistenceConsumer  = "chat-persistence"
	dlqStream            = "CHAT_PERSIST_DLQ"
	dlqSubject           = "chat.dlq.persistence"
	persistenceBatchSize = 500
	persistenceBatchWait = 100 * time.Millisecond
)

type circuitState uint8

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

type Persistence struct {
	conn       *nats.Conn
	js         jetstream.JetStream
	consumer   jetstream.Consumer
	cfg        config.PersistenceConfig
	repo       Repository
	ping       func(context.Context) error
	queryReady atomic.Bool
	mu         sync.Mutex
	state      circuitState
	failures   int
	trial      bool
	wake       chan struct{}
}

func NewPersistence(ctx context.Context, conn *nats.Conn, repo Repository, cfg config.PersistenceConfig, ping func(context.Context) error) (*Persistence, error) {
	if cfg.AckWait <= cfg.WriteTimeout+persistenceBatchWait {
		return nil, errors.New("ACK_WAIT must exceed WRITE_TIMEOUT plus batch wait")
	}
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, err
	}
	for _, streamConfig := range []jetstream.StreamConfig{
		{
			Name:     dlqStream,
			Subjects: []string{dlqSubject},
			Storage:  jetstream.FileStorage,
			Replicas: 1,
			Discard:  jetstream.DiscardNew,
			MaxBytes: cfg.DLQMaxBytes,
		},
		{
			Name:       persistenceStream,
			Subjects:   []string{persistenceSubject},
			Retention:  jetstream.WorkQueuePolicy,
			Storage:    jetstream.FileStorage,
			Replicas:   1,
			Discard:    jetstream.DiscardNew,
			MaxBytes:   cfg.MaxBytes,
			Duplicates: 2 * time.Minute,
			RePublish: &jetstream.RePublish{
				Source:      persistenceSubject,
				Destination: "room.msg.$1",
			},
		},
	} {
		if _, err := js.CreateOrUpdateStream(ctx, streamConfig); err != nil {
			return nil, fmt.Errorf("configure %s: %w", streamConfig.Name, err)
		}
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, persistenceStream, jetstream.ConsumerConfig{
		Name:          persistenceConsumer,
		Durable:       persistenceConsumer,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    -1,
		MaxAckPending: cfg.MaxAckPending,
		FilterSubject: persistenceSubject,
	})
	if err != nil {
		return nil, err
	}
	return &Persistence{
		conn:     conn,
		js:       js,
		consumer: consumer,
		cfg:      cfg,
		repo:     repo,
		ping:     ping,
		state:    circuitOpen,
		wake:     make(chan struct{}),
	}, nil
}

func (p *Persistence) Connected() bool  { return p.conn.IsConnected() }
func (p *Persistence) QueryReady() bool { return p.queryReady.Load() }

func (p *Persistence) Run(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error { p.probeMongo(ctx); return nil })
	for range p.cfg.Workers {
		eg.Go(func() error { p.runWorker(ctx); return nil })
	}
	return eg.Wait()
}

func (p *Persistence) signalLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}

func (p *Persistence) acquireWorker(ctx context.Context) (trial, ok bool) {
	for {
		p.mu.Lock()
		if p.state == circuitClosed {
			p.mu.Unlock()
			return false, true
		}
		if p.state == circuitHalfOpen && !p.trial {
			p.trial = true
			p.mu.Unlock()
			return true, true
		}
		wake := p.wake
		p.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return false, false
		}
	}
}

func (p *Persistence) recordBatchResult(success, trial bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if success {
		if trial && p.state == circuitHalfOpen {
			p.state = circuitClosed
			p.trial = false
			p.signalLocked()
		}
		if p.state == circuitClosed {
			p.failures = 0
		}
		return
	}
	p.failures++
	if trial || p.failures >= 3 {
		p.state = circuitOpen
		p.trial = false
		p.signalLocked()
	}
}

func (p *Persistence) probeMongo(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := p.ping(pingCtx)
		cancel()
		p.queryReady.Store(err == nil)
		p.mu.Lock()
		if err == nil && p.state == circuitOpen {
			p.state = circuitHalfOpen
			p.trial = false
			p.signalLocked()
		}
		open := p.state == circuitOpen
		p.mu.Unlock()
		p.observeState(ctx)
		delay := 5 * time.Second
		if open || err != nil {
			attempt++
			delay = retryDelay(attempt, []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second})
		} else {
			attempt = 0
		}
		if !wait(ctx, delay) {
			return
		}
	}
}

func (p *Persistence) observeState(ctx context.Context) {
	p.mu.Lock()
	state := p.state
	p.mu.Unlock()
	chatCircuit.Record(ctx, int64(state))
	chatWorkers.Record(ctx, int64(p.cfg.Workers))
	infoCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if info, err := p.consumer.Info(infoCtx); err == nil {
		ackPending := int64(max(info.NumAckPending, 0))
		pending := int64(min(info.NumPending, math.MaxInt64))
		chatPending.Record(ctx, pending+min(ackPending, math.MaxInt64-pending))
	}
	stream, err := p.js.Stream(infoCtx, persistenceStream)
	if err != nil {
		return
	}
	info, err := stream.Info(infoCtx)
	if err != nil {
		return
	}
	age := 0.0
	if info.State.Msgs > 0 {
		age = time.Since(info.State.FirstTime).Seconds()
	}
	chatOldestPending.Record(ctx, age)
}

func (p *Persistence) releaseTrial(trial bool) {
	if !trial {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == circuitHalfOpen {
		p.trial = false
		p.signalLocked()
	}
}

func (p *Persistence) runWorker(ctx context.Context) {
	for ctx.Err() == nil {
		trial, ok := p.acquireWorker(ctx)
		if !ok {
			return
		}
		batch, err := p.consumer.Fetch(persistenceBatchSize, jetstream.FetchMaxWait(persistenceBatchWait))
		if err != nil {
			p.releaseTrial(trial)
			slog.WarnContext(ctx, "persistence fetch failed", "error", err)
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		var deliveries []jetstream.Msg
		for msg := range batch.Messages() {
			deliveries = append(deliveries, msg)
		}
		if len(deliveries) == 0 {
			p.releaseTrial(trial)
			if err := batch.Error(); err != nil {
				slog.WarnContext(ctx, "persistence fetch failed", "error", err)
				if !wait(ctx, time.Second) {
					return
				}
			}
			continue
		}
		if ctx.Err() != nil {
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.WriteTimeout)
			p.saveBatch(drainCtx, deliveries, trial)
			cancel()
			return
		}
		p.saveBatch(ctx, deliveries, trial)
	}
}

func (p *Persistence) saveBatch(ctx context.Context, deliveries []jetstream.Msg, trial bool) {
	var msgs []*Message
	var valid []jetstream.Msg
	for _, d := range deliveries {
		var msg Message
		if err := json.Unmarshal(d.Data(), &msg); err != nil {
			p.deadLetter(ctx, d, "decode", err)
			continue
		}
		setCreatedAtFromID(&msg)
		if !validPersistenceMessage(&msg) {
			p.deadLetter(ctx, d, "validation", ErrInvalidMessage)
			continue
		}
		msgs = append(msgs, &msg)
		valid = append(valid, d)
	}
	if len(msgs) == 0 {
		p.releaseTrial(trial)
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, p.cfg.WriteTimeout)
	results := p.repo.SaveBatch(writeCtx, msgs)
	cancel()
	chatBatchSize.Record(ctx, float64(len(msgs)))
	success := true
	for i, err := range results {
		d := valid[i]
		switch {
		case err == nil:
			if err := d.Ack(); err != nil {
				slog.WarnContext(ctx, "persistence ACK failed", "error", err)
			}
			chatMessagesSavedTotal.Add(ctx, 1)
			chatPersistenceLag.Record(ctx, time.Since(msgs[i].CreatedAt).Seconds())
		case errors.Is(err, ErrInvalidMessage), errors.Is(err, ErrMessageConflict):
			p.deadLetter(ctx, d, "permanent", err)
		default:
			if success {
				slog.WarnContext(ctx, "persistence batch will be retried", "batch_size", len(msgs), "error", err)
			}
			success = false
			attempt := 1
			if md, me := d.Metadata(); me == nil {
				attempt = int(min(md.NumDelivered, math.MaxInt))
			}
			p.retryMessage(ctx, d, retryDelay(attempt, []time.Duration{250 * time.Millisecond, time.Second, 3 * time.Second}))
		}
	}
	p.recordBatchResult(success, trial)
}

func setCreatedAtFromID(msg *Message) {
	if !msg.CreatedAt.IsZero() {
		return
	}
	id, err := uuid.Parse(msg.ID)
	if err == nil && id.Version() == 7 {
		msg.CreatedAt = time.Unix(id.Time().UnixTime())
	}
}

func validPersistenceMessage(msg *Message) bool {
	if msg.Content == "" || msg.Type != "chat" || msg.CreatedAt.IsZero() {
		return false
	}
	for _, value := range []string{msg.ID, msg.RoomID, msg.SenderID, msg.ClientMsgID} {
		id, err := uuid.Parse(value)
		if err != nil || id.String() != value {
			return false
		}
		if value == msg.ID && id.Version() != 7 {
			return false
		}
	}
	return true
}

func (p *Persistence) retryMessage(ctx context.Context, msg jetstream.Msg, delay time.Duration) {
	chatRetryTotal.Add(ctx, 1)
	if err := msg.NakWithDelay(delay); err != nil {
		slog.WarnContext(ctx, "persistence NAK failed", "error", err)
	}
}

func (p *Persistence) deadLetter(ctx context.Context, msg jetstream.Msg, reason string, cause error) {
	data, err := json.Marshal(struct {
		Original []byte `json:"original"`
		Reason   string `json:"reason"`
		Error    string `json:"error"`
	}{
		Original: msg.Data(),
		Reason:   reason,
		Error:    cause.Error(),
	})
	if err != nil {
		return
	}
	dlqCtx, cancel := context.WithTimeout(ctx, p.cfg.WriteTimeout)
	defer cancel()
	var opts []jetstream.PublishOpt
	if md, err := msg.Metadata(); err == nil {
		opts = append(opts, jetstream.WithMsgID(fmt.Sprintf("%s:%d", md.Stream, md.Sequence.Stream)))
	}
	if _, err := p.js.Publish(dlqCtx, dlqSubject, data, opts...); err != nil {
		slog.ErrorContext(ctx, "DLQ publish failed", "error", err)
		p.retryMessage(ctx, msg, 30*time.Second)
		return
	}
	chatDLQTotal.Add(ctx, 1)
	if err := msg.Ack(); err != nil {
		slog.WarnContext(ctx, "DLQ original ACK failed", "error", err)
	}
}

func retryDelay(attempt int, caps []time.Duration) time.Duration {
	index := min(max(attempt-1, 0), len(caps)-1)
	// #nosec G404 -- Retry jitter does not require cryptographic randomness.
	return time.Duration(rand.Int64N(int64(caps[index])))
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
