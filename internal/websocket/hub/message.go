package hub

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	msgTypeChat   = "chat"
	msgTypeSystem = "system"
)

type Message struct {
	ID          string `json:"id"`
	RoomID      string `json:"room_id"`
	SenderID    string `json:"sender_id"`
	Content     string `json:"content"`
	ClientMsgID string `json:"client_msg_id,omitempty"`
	Type        string `json:"type"`
	Timestamp   int64  `json:"timestamp,omitempty"`

	ReceivedAt time.Time `json:"-"`
}

type incomingRequest struct {
	Content     string `json:"content"`
	ClientMsgID string `json:"client_msg_id"`
	Type        string `json:"type,omitempty"`
}

func NewSystemMessage(roomID, content string) (*Message, error) {
	clientMsgID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate client message ID: %w", err)
	}

	return &Message{
		RoomID:      roomID,
		SenderID:    systemSenderID,
		Content:     content,
		ClientMsgID: clientMsgID.String(),
		Type:        msgTypeSystem,
		Timestamp:   time.Now().Unix(),
	}, nil
}

func (m *Message) toRawJSON() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	return data, nil
}

func uuidV7Gap(newerID, olderID string) (time.Duration, bool) {
	newer, err := uuid.Parse(newerID)
	if err != nil || newer.Version() != 7 {
		return 0, false
	}
	older, err := uuid.Parse(olderID)
	if err != nil || older.Version() != 7 {
		return 0, false
	}
	return time.Unix(newer.Time().UnixTime()).Sub(time.Unix(older.Time().UnixTime())), true
}
