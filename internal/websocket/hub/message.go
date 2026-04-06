package hub

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	msgTypeChat     = "chat"
	msgTypeConflict = "conflict"
	msgTypeSystem   = "system"
)

type Message struct {
	ID             string `json:"id"`
	RoomID         string `json:"room_id"`
	SenderID       string `json:"sender_id"`
	Content        string `json:"content"`
	Type           string `json:"type"`
	ClientMsgID    string `json:"client_msg_id,omitempty"`
	SequenceNumber int64  `json:"sequence_number"`
	Timestamp      int64  `json:"timestamp,omitempty"`

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
		Type:        msgTypeSystem,
		Timestamp:   time.Now().Unix(),
		ClientMsgID: clientMsgID.String(),
	}, nil
}

func (m *Message) toRawJSON() ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	return data, nil
}
