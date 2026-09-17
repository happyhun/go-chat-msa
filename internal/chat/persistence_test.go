package chat

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type persistenceDelivery struct {
	jetstream.Msg
	data  []byte
	acked bool
	naked bool
}

func (d *persistenceDelivery) Data() []byte                     { return d.data }
func (d *persistenceDelivery) Ack() error                       { d.acked = true; return nil }
func (d *persistenceDelivery) NakWithDelay(time.Duration) error { d.naked = true; return nil }
func (d *persistenceDelivery) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: 1}, nil
}

type persistenceRepository struct {
	Repository
	results []error
}

func (r persistenceRepository) SaveBatch(context.Context, []*Message) []error { return r.results }

type persistenceJetStream struct{ jetstream.JetStream }

func (persistenceJetStream) Publish(context.Context, string, []byte, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return &jetstream.PubAck{}, nil
}

func TestPersistenceSaveAcknowledgesOnlyConfirmedMessages(t *testing.T) {
	var deliveries []*persistenceDelivery
	for range 2 {
		id, err := uuid.NewV7()
		require.NoError(t, err)
		data, err := json.Marshal(&Message{ID: id.String(), RoomID: uuid.NewString(), SenderID: uuid.NewString(), ClientMsgID: uuid.NewString(), Content: "hello", Type: "chat", CreatedAt: time.Now()})
		require.NoError(t, err)
		deliveries = append(deliveries, &persistenceDelivery{data: data})
	}
	p := &Persistence{repo: persistenceRepository{results: []error{nil, errors.New("network timeout")}}}
	p.cfg.WriteTimeout = time.Second
	p.saveBatch(t.Context(), []jetstream.Msg{deliveries[0], deliveries[1]}, false)
	require.Equal(t, 1, p.failures)
	require.True(t, deliveries[0].acked)
	require.False(t, deliveries[0].naked)
	require.False(t, deliveries[1].acked)
	require.True(t, deliveries[1].naked)
}

func TestPersistenceDerivesCreatedAtFromWebSocketMessageID(t *testing.T) {
	id, err := uuid.NewV7()
	require.NoError(t, err)
	msg := &Message{ID: id.String()}

	setCreatedAtFromID(msg)

	require.Equal(t, time.Unix(id.Time().UnixTime()), msg.CreatedAt)
}

func TestPersistenceMalformedBatchDoesNotCloseHalfOpenCircuit(t *testing.T) {
	p := &Persistence{js: persistenceJetStream{}, wake: make(chan struct{}), state: circuitHalfOpen, trial: true}
	p.cfg.WriteTimeout = time.Second
	delivery := &persistenceDelivery{data: []byte("malformed")}
	p.saveBatch(t.Context(), []jetstream.Msg{delivery}, true)
	require.True(t, delivery.acked)
	require.Equal(t, circuitHalfOpen, p.state)
	require.False(t, p.trial)
	trial, ok := p.acquireWorker(t.Context())
	require.True(t, trial)
	require.True(t, ok)
}

func TestPersistenceCircuitAllowsSingleHalfOpenWorker(t *testing.T) {
	p := &Persistence{wake: make(chan struct{})}
	for range 3 {
		p.recordBatchResult(false, false)
	}
	require.Equal(t, circuitOpen, p.state)
	p.mu.Lock()
	p.state = circuitHalfOpen
	p.signalLocked()
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var claimed atomic.Int32
	for range 4 {
		go func() {
			trial, ok := p.acquireWorker(ctx)
			if ok && trial {
				claimed.Add(1)
			}
		}()
	}
	require.Eventually(t, func() bool { return claimed.Load() == 1 }, time.Second, time.Millisecond)
	p.recordBatchResult(true, true)
	require.Equal(t, circuitClosed, p.state)
}

func TestPersistenceFetchFailureDoesNotOpenMongoCircuit(t *testing.T) {
	p := &Persistence{state: circuitHalfOpen, trial: true, wake: make(chan struct{})}
	p.releaseTrial(true)
	require.Equal(t, circuitHalfOpen, p.state)
	require.False(t, p.trial)
	require.Zero(t, p.failures)
}

func TestRetryDelayIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 100; attempt++ {
		delay := retryDelay(attempt, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second})
		require.GreaterOrEqual(t, delay, time.Duration(0))
		require.LessOrEqual(t, delay, 3*time.Second)
	}
}
