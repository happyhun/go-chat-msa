package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Repository interface {
	SaveBatch(ctx context.Context, msgs []*Message) []error
	GetHistory(ctx context.Context, roomID string, limit int64, joinedAt time.Time) ([]*Message, error)
	SyncMessages(ctx context.Context, roomID, afterID string, limit int64, joinedAt time.Time) ([]*Message, error)
}

type collection interface {
	InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error)
	Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*mongo.Cursor, error)
	FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *mongo.SingleResult
}

type mongoRepository struct {
	col collection
}

func NewRepository(col collection) Repository {
	return &mongoRepository{col: col}
}

func (r *mongoRepository) SaveBatch(ctx context.Context, msgs []*Message) []error {
	results := make([]error, len(msgs))
	if len(msgs) == 0 {
		return results
	}

	docs := make([]any, len(msgs))
	for i, m := range msgs {
		docs[i] = m
	}

	opts := options.InsertMany().SetOrdered(false)
	_, err := r.col.InsertMany(ctx, docs, opts)

	if err == nil {
		return results
	}
	bulkErr, ok := errors.AsType[mongo.BulkWriteException](err)
	if !ok || bulkErr.WriteConcernError != nil || len(bulkErr.WriteErrors) == 0 {
		for i := range results {
			results[i] = err
		}
		return results
	}
	for _, we := range bulkErr.WriteErrors {
		if we.Index < 0 || we.Index >= len(msgs) {
			for i := range results {
				results[i] = err
			}
			return results
		}
		switch we.Code {
		case 11000:
			results[we.Index] = r.checkDuplicate(ctx, msgs[we.Index])
		case 121:
			results[we.Index] = fmt.Errorf("%w: %s", ErrInvalidMessage, we.Message)
		default:
			results[we.Index] = we
		}
	}
	return results
}

var (
	ErrMessageConflict = errors.New("message payload conflicts with existing document")
	ErrInvalidMessage  = errors.New("invalid message")
)

func (r *mongoRepository) checkDuplicate(ctx context.Context, msg *Message) error {
	filter := bson.M{"$or": bson.A{
		bson.M{"_id": msg.ID},
		bson.M{"roomId": msg.RoomID, "senderId": msg.SenderID, "clientMsgId": msg.ClientMsgID},
	}}
	var existing Message
	if err := r.col.FindOne(ctx, filter).Decode(&existing); err != nil {
		return err
	}
	if existing.RoomID != msg.RoomID || existing.SenderID != msg.SenderID ||
		existing.ClientMsgID != msg.ClientMsgID || existing.Type != msg.Type || existing.Content != msg.Content {
		return ErrMessageConflict
	}
	return nil
}

func (r *mongoRepository) GetHistory(ctx context.Context, roomID string, limit int64, joinedAt time.Time) ([]*Message, error) {
	filter := bson.M{"roomId": roomID}

	if !joinedAt.IsZero() {
		filter["createdAt"] = bson.M{"$gte": joinedAt}
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "_id", Value: -1}}).
		SetLimit(limit)

	return r.find(ctx, filter, opts)
}

func (r *mongoRepository) SyncMessages(ctx context.Context, roomID, afterID string, limit int64, joinedAt time.Time) ([]*Message, error) {
	filter := bson.M{
		"roomId": roomID,
		"_id":    bson.M{"$gt": afterID},
	}

	if !joinedAt.IsZero() {
		filter["createdAt"] = bson.M{"$gte": joinedAt}
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetLimit(limit)

	return r.find(ctx, filter, opts)
}

func (r *mongoRepository) find(ctx context.Context, filter any, opts *options.FindOptions) ([]*Message, error) {
	cursor, err := r.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var messages []*Message
	if err := cursor.All(ctx, &messages); err != nil {
		return nil, err
	}

	return messages, nil
}
