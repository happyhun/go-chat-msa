package websocket

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	chatpb "go-chat-msa/api/proto/chat/v1"
	"go-chat-msa/internal/websocket/hub"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	retryTickInterval = 500 * time.Millisecond
	retryQueueSize    = 250
	retryMaxAttempts  = 5
	retryInitBackoff  = 1 * time.Second
	retryMaxBackoff   = 30 * time.Second
)

type batchRetryTask struct {
	msgs      []*hub.Message
	attempts  int
	nextRetry time.Time
	createdAt time.Time
}

type chatStoreAdapter struct {
	client     chatpb.ChatServiceClient
	rpcTimeout time.Duration
	retryCh    chan batchRetryTask
}

func newChatStoreAdapter(client chatpb.ChatServiceClient, rpcTimeout time.Duration) *chatStoreAdapter {
	return &chatStoreAdapter{
		client:     client,
		rpcTimeout: rpcTimeout,
		retryCh:    make(chan batchRetryTask, retryQueueSize),
	}
}

func (a *chatStoreAdapter) GetLastSequenceNumber(ctx context.Context, roomID string) (int64, error) {
	resp, err := a.client.GetLastSequenceNumber(ctx, &chatpb.GetLastSequenceNumberRequest{
		RoomId: roomID,
	})
	if err != nil {
		return 0, err
	}
	return resp.SequenceNumber, nil
}

func (a *chatStoreAdapter) SaveMany(ctx context.Context, msgs []*hub.Message) {
	if err := a.doSaveMany(ctx, msgs); err != nil {
		if !isRetryable(err) {
			slog.ErrorContext(ctx, "batch save permanently failed (non-retryable)",
				"count", len(msgs), "error", err)
			persistenceBatchSaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "reject")))
			return
		}
		a.enqueueBatchRetry(ctx, msgs)
		return
	}
	persistenceBatchSaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
}

func (a *chatStoreAdapter) doSaveMany(ctx context.Context, msgs []*hub.Message) error {
	ctx, cancel := context.WithTimeout(ctx, a.rpcTimeout)
	defer cancel()

	reqs := make([]*chatpb.CreateMessageRequest, len(msgs))
	for i, msg := range msgs {
		reqs[i] = &chatpb.CreateMessageRequest{
			RoomId:         msg.RoomID,
			SenderId:       msg.SenderID,
			Content:        msg.Content,
			ClientMsgId:    msg.ClientMsgID,
			Type:           msg.Type,
			SequenceNumber: msg.SequenceNumber,
			MessageId:      msg.ID,
		}
	}
	_, err := a.client.BatchCreateMessages(ctx, &chatpb.BatchCreateMessagesRequest{Requests: reqs})
	return err
}

func (a *chatStoreAdapter) enqueueBatchRetry(ctx context.Context, msgs []*hub.Message) {
	now := time.Now()
	task := batchRetryTask{
		msgs:      msgs,
		attempts:  1,
		nextRetry: now.Add(a.jitteredBackoff(1)),
		createdAt: now,
	}
	select {
	case a.retryCh <- task:
		persistenceRetryQueueDepth.Record(ctx, float64(len(a.retryCh)))
		persistenceBatchSaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "retry")))
		slog.WarnContext(ctx, "batch enqueued for retry", "count", len(msgs))
	default:
		persistenceRetryQueueFullTotal.Add(ctx, 1)
		slog.ErrorContext(ctx, "retry queue full, batch dropped", "count", len(msgs))
	}
}

func (a *chatStoreAdapter) runRetryWorker(ctx context.Context) {
	slog.InfoContext(ctx, "retry worker started")
	defer slog.InfoContext(ctx, "retry worker stopped")

	var batch []batchRetryTask
	ticker := time.NewTicker(retryTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.drainBatch(context.Background(), batch)
			return
		case task := <-a.retryCh:
			batch = append(batch, task)
			persistenceRetryQueueDepth.Record(ctx, float64(len(a.retryCh)))
		case <-ticker.C:
			if len(batch) == 0 {
				continue
			}
			a.updateBatchMetrics(batch)
			batch = a.processBatch(ctx, batch)
		}
	}
}

func (a *chatStoreAdapter) updateBatchMetrics(batch []batchRetryTask) {
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

func (a *chatStoreAdapter) processBatch(ctx context.Context, batch []batchRetryTask) []batchRetryTask {
	now := time.Now()
	remaining := batch[:0]

	for _, task := range batch {
		if now.Before(task.nextRetry) {
			remaining = append(remaining, task)
			continue
		}

		err := a.doSaveMany(ctx, task.msgs)
		if err == nil {
			persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
			slog.InfoContext(ctx, "batch retry succeeded",
				"count", len(task.msgs), "attempts", task.attempts)
			continue
		}

		if st, ok := status.FromError(err); ok && st.Code() == codes.AlreadyExists {
			persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
			slog.InfoContext(ctx, "batch retry idempotent success",
				"count", len(task.msgs))
			continue
		}

		if !isRetryable(err) {
			persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "reject")))
			slog.ErrorContext(ctx, "batch retry permanently failed (non-retryable)",
				"error", err, "count", len(task.msgs))
			continue
		}

		task.attempts++
		if task.attempts > retryMaxAttempts {
			persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "exhaust")))
			slog.ErrorContext(ctx, "batch exhausted all retries",
				"count", len(task.msgs), "attempts", task.attempts-1)
			continue
		}

		persistenceRetrySaveTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "retry")))
		task.nextRetry = now.Add(a.jitteredBackoff(task.attempts))
		remaining = append(remaining, task)
	}

	return remaining
}

func (a *chatStoreAdapter) drainBatch(ctx context.Context, batch []batchRetryTask) {
drain:
	for {
		select {
		case task := <-a.retryCh:
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
		if err := a.doSaveMany(ctx, task.msgs); err != nil {
			slog.ErrorContext(ctx, "failed to save batch during shutdown drain",
				"error", err, "count", len(task.msgs))
		}
	}
}

func (a *chatStoreAdapter) jitteredBackoff(attempts int) time.Duration {
	base := min(retryInitBackoff*(1<<min(attempts-1, 5)), retryMaxBackoff)
	return time.Duration(rand.Int64N(int64(base)))
}

func isRetryable(err error) bool {
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
