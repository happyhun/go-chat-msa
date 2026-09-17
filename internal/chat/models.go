package chat

import (
	"time"
)

type Message struct {
	ID          string    `bson:"_id,omitempty" json:"id"`
	RoomID      string    `bson:"roomId" json:"room_id"`
	SenderID    string    `bson:"senderId" json:"sender_id"`
	Content     string    `bson:"content" json:"content"`
	ClientMsgID string    `bson:"clientMsgId" json:"client_msg_id"`
	Type        string    `bson:"type" json:"type"`
	CreatedAt   time.Time `bson:"createdAt" json:"created_at"`
}
