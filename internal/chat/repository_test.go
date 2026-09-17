package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type batchCollection struct {
	collection
	err       error
	existing  *Message
	lookupErr error
	ordered   bool
}

func (c *batchCollection) InsertMany(_ context.Context, _ []any, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error) {
	c.ordered = *opts[0].Ordered
	return nil, c.err
}

func (c *batchCollection) FindOne(context.Context, any, ...*options.FindOneOptions) *mongo.SingleResult {
	return mongo.NewSingleResultFromDocument(c.existing, c.lookupErr, bson.NewRegistry())
}

func TestRepositorySaveBatch(t *testing.T) {
	transient := errors.New("network timeout")
	message := &Message{ID: "id", RoomID: "room", SenderID: "sender", ClientMsgID: "client", Content: "hello", Type: "chat"}
	conflict := *message
	conflict.Content = "different"
	tests := []struct {
		name     string
		err      error
		existing *Message
		want     []error
	}{
		{"success", nil, nil, []error{nil, nil}},
		{"ambiguous network failure", transient, nil, []error{transient, transient}},
		{"partial validation failure", mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{Index: 1, Code: 121}}}, nil, []error{nil, ErrInvalidMessage}},
		{"matching duplicate", mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{Index: 0, Code: 11000}}}, message, []error{nil, nil}},
		{"conflicting duplicate", mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{Index: 0, Code: 11000}}}, &conflict, []error{ErrMessageConflict, nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := &batchCollection{err: tt.err, existing: tt.existing}
			results := NewRepository(col).SaveBatch(t.Context(), []*Message{message, message})
			require.False(t, col.ordered)
			require.Len(t, results, len(tt.want))
			for i, want := range tt.want {
				require.ErrorIs(t, results[i], want)
			}
		})
	}
}

func TestRepositorySaveBatchWriteConcernFailure(t *testing.T) {
	err := mongo.BulkWriteException{WriteConcernError: &mongo.WriteConcernError{Code: 64, Message: "journal timeout"}, WriteErrors: []mongo.BulkWriteError{{Index: 0, Code: 11000}}}
	results := NewRepository(&batchCollection{err: err}).SaveBatch(t.Context(), []*Message{{ID: "one"}, {ID: "two"}})
	for _, result := range results {
		require.Error(t, result)
	}
}

func TestRepositorySaveBatchDuplicateWithNewID(t *testing.T) {
	existing := &Message{ID: "old", RoomID: "room", SenderID: "sender", ClientMsgID: "client", Content: "hello", Type: "chat"}
	message := *existing
	message.ID = "new"
	col := &batchCollection{existing: existing, err: mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{Index: 0, Code: 11000}}}}
	require.NoError(t, NewRepository(col).SaveBatch(t.Context(), []*Message{&message})[0])
}
