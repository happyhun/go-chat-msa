package natsbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go-chat-msa/internal/websocket/hub"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const (
	subjectRoomMsgPrefix   = "room.msg."
	subjectPersistPrefix   = "chat.persist."
	subjectRoomEventPrefix = "room.event."
	subjectRoomEventAll    = "room.event.*"

	headerReceivedAt = "Gochat-Received-At"
	headerSenderID   = "Gochat-Sender-Id"
	headerOriginPod  = "Gochat-Origin-Pod"
	headerMessageID  = "Gochat-Message-Id"
)

var (
	ErrURLRequired = errors.New("nats url is required")
	ErrInvalidRoom = errors.New("room id must be a canonical uuid")
)

type Config struct {
	URL             string
	Name            string
	FlushTimeout    time.Duration
	SubPendingMsgs  int
	SubPendingBytes int
}

type Bus struct {
	conn *nats.Conn
	js   jetstream.JetStream
	cfg  Config

	mu        sync.RWMutex
	observer  hub.BusObserver
	resetting atomic.Bool
}

func Connect(cfg Config) (*Bus, error) {
	if cfg.URL == "" {
		return nil, ErrURLRequired
	}

	b := &Bus{cfg: cfg}

	conn, err := nats.Connect(cfg.URL,
		nats.Name(cfg.Name),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.ReconnectBufSize(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			b.resetting.Store(true)
			slog.WarnContext(context.Background(), "NATS disconnected", "error", err)
			if observer := b.currentObserver(); observer != nil {
				observer.OnDisconnected()
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			b.resetting.Store(false)
			slog.InfoContext(context.Background(), "NATS reconnected", "url", nc.ConnectedUrl())
			if observer := b.currentObserver(); observer != nil {
				observer.OnReconnected()
			}
		}),
		nats.ErrorHandler(b.handleAsyncError),
	)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}

	b.conn = conn
	b.js, err = jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("initialize jetstream: %w", err)
	}
	return b, nil
}

func (b *Bus) SetObserver(o hub.BusObserver) {
	b.mu.Lock()
	b.observer = o
	b.mu.Unlock()

	if o == nil {
		return
	}
	if b.Connected() {
		o.OnReconnected()
		return
	}
	o.OnDisconnected()
}

func (b *Bus) currentObserver() hub.BusObserver {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.observer
}

func (b *Bus) handleAsyncError(nc *nats.Conn, sub *nats.Subscription, err error) {
	subject := ""
	dropped := 0
	if sub != nil {
		subject = sub.Subject
		if n, dropErr := sub.Dropped(); dropErr == nil {
			dropped = n
		}
	}
	slog.ErrorContext(context.Background(), "NATS async error", "subject", subject, "error", err)

	if !errors.Is(err, nats.ErrSlowConsumer) {
		return
	}
	if observer := b.currentObserver(); observer != nil {
		observer.OnSlowConsumer(dropped)
	}
	if !nc.IsConnected() || !b.resetting.CompareAndSwap(false, true) {
		return
	}
	if err := nc.ForceReconnect(); err != nil {
		b.resetting.Store(false)
		slog.ErrorContext(context.Background(), "reset NATS connection after slow consumer", "error", err)
	}
}

func (b *Bus) PublishMessage(ctx context.Context, env hub.Envelope) error {
	subject, err := roomMessageSubject(env.RoomID)
	if err != nil {
		return err
	}
	if env.Persistent {
		subject = subjectPersistPrefix + env.RoomID
	}

	msg := &nats.Msg{
		Subject: subject,
		Data:    env.Payload,
		Header:  nats.Header{},
	}
	msg.Header.Set(headerReceivedAt, strconv.FormatInt(env.ReceivedAt.UnixNano(), 10))
	msg.Header.Set(headerSenderID, env.SenderID)
	msg.Header.Set(headerOriginPod, env.OriginPod)
	msg.Header.Set(headerMessageID, env.MessageID)

	if env.Persistent {
		_, err = b.js.PublishMsg(ctx, msg, jetstream.WithMsgID(env.DedupID))
	} else {
		err = b.conn.PublishMsg(msg)
	}
	if err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

func (b *Bus) SubscribeRoom(_ context.Context, roomID string, deliver func(hub.Envelope)) (hub.Subscription, error) {
	subject, err := roomMessageSubject(roomID)
	if err != nil {
		return nil, err
	}

	sub, err := b.conn.Subscribe(subject, func(msg *nats.Msg) {
		deliver(envelopeFromMsg(roomID, msg))
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", subject, err)
	}

	if b.cfg.SubPendingMsgs > 0 && b.cfg.SubPendingBytes > 0 {
		if err := sub.SetPendingLimits(b.cfg.SubPendingMsgs, b.cfg.SubPendingBytes); err != nil {
			_ = sub.Unsubscribe()
			return nil, fmt.Errorf("set pending limits %s: %w", subject, err)
		}
	}

	if err := b.conn.FlushTimeout(b.cfg.FlushTimeout); err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("flush subscription %s: %w", subject, err)
	}

	return sub, nil
}

func (b *Bus) PublishEvent(_ context.Context, ev hub.RoomEvent) error {
	subject, err := roomEventSubject(ev.RoomID)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal room event: %w", err)
	}
	if err := b.conn.Publish(subject, payload); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

func (b *Bus) SubscribeEvents(deliver func(hub.RoomEvent)) (hub.Subscription, error) {
	sub, err := b.conn.Subscribe(subjectRoomEventAll, func(msg *nats.Msg) {
		ev, ok := roomEventFromMsg(msg)
		if !ok {
			if observer := b.currentObserver(); observer != nil {
				observer.OnInvalidEvent()
			}
			return
		}
		deliver(ev)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", subjectRoomEventAll, err)
	}

	if err := b.conn.FlushTimeout(b.cfg.FlushTimeout); err != nil {
		if b.conn.IsReconnecting() {
			return sub, nil
		}
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("flush event subscription: %w", err)
	}
	return sub, nil
}

func (b *Bus) Connected() bool {
	return b != nil && b.conn != nil && !b.resetting.Load() && b.conn.IsConnected()
}

func (b *Bus) Drain() error {
	if b == nil || b.conn == nil {
		return nil
	}
	if err := b.conn.Drain(); err != nil {
		return fmt.Errorf("drain nats: %w", err)
	}
	return nil
}

func envelopeFromMsg(roomID string, msg *nats.Msg) hub.Envelope {
	env := hub.Envelope{
		RoomID:  roomID,
		Payload: msg.Data,
	}
	if msg.Header == nil {
		invalidHeadersTotal.Add(context.Background(), 1)
		return env
	}

	env.SenderID = msg.Header.Get(headerSenderID)
	env.OriginPod = msg.Header.Get(headerOriginPod)
	env.MessageID = msg.Header.Get(headerMessageID)
	raw := msg.Header.Get(headerReceivedAt)
	nanos, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		invalidHeadersTotal.Add(context.Background(), 1)
		return env
	}
	env.ReceivedAt = time.Unix(0, nanos)
	return env
}

func roomMessageSubject(roomID string) (string, error) {
	if err := validateRoomID(roomID); err != nil {
		return "", err
	}
	return subjectRoomMsgPrefix + roomID, nil
}

func roomEventSubject(roomID string) (string, error) {
	if err := validateRoomID(roomID); err != nil {
		return "", err
	}
	return subjectRoomEventPrefix + roomID, nil
}

func roomEventFromMsg(msg *nats.Msg) (hub.RoomEvent, bool) {
	var event hub.RoomEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil || event.Type == "" {
		return event, false
	}
	subject, err := roomEventSubject(event.RoomID)
	return event, err == nil && msg.Subject == subject
}

func validateRoomID(roomID string) error {
	parsed, err := uuid.Parse(roomID)
	if err != nil || parsed.String() != roomID {
		return fmt.Errorf("%w: %q", ErrInvalidRoom, roomID)
	}
	return nil
}

var (
	busMeter            = otel.Meter("go-chat-msa/websocket/natsbus")
	invalidHeadersTotal metric.Int64Counter
)

func init() {
	var err error
	invalidHeadersTotal, err = busMeter.Int64Counter("gochat_ws_nats_invalid_headers",
		metric.WithDescription("헤더가 없거나 형식이 틀린 메시지 수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_ws_nats_invalid_headers", "error", err)
	}
}
