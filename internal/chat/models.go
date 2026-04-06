package chat

import (
	"time"
)

type Message struct {
	ID             string    `bson:"_id,omitempty"`
	RoomID         string    `bson:"roomId"`
	SenderID       string    `bson:"senderId"`
	Content        string    `bson:"content"`
	Type           string    `bson:"type"`
	ClientMsgID    string    `bson:"clientMsgId"`
	SequenceNumber int64     `bson:"sequenceNumber"`
	CreatedAt      time.Time `bson:"createdAt"`
}
