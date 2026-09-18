package hub

import (
	"context"
	"time"
)

type Envelope struct {
	RoomID     string
	MessageID  string
	DedupID    string
	SenderID   string
	OriginPod  string
	ReceivedAt time.Time
	Payload    []byte
	Persistent bool
}

type EventType string

const (
	EventRoomClosed EventType = "room_closed"
)

type RoomEvent struct {
	Type       EventType `json:"type"`
	RoomID     string    `json:"room_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

type Subscription interface {
	Unsubscribe() error
}

type MessageBus interface {
	PublishMessage(ctx context.Context, env Envelope) error
	SubscribeRoom(ctx context.Context, roomID string, deliver func(Envelope)) (Subscription, error)
	PublishEvent(ctx context.Context, ev RoomEvent) error
	SubscribeEvents(deliver func(RoomEvent)) (Subscription, error)
	Connected() bool
}

type BusObserver interface {
	OnSlowConsumer(dropped int)
	OnInvalidEvent()
	OnDisconnected()
	OnReconnected()
}
