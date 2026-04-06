package chat

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Repository interface {
	SaveMany(ctx context.Context, msgs []*Message) error
	GetHistory(ctx context.Context, roomID string, limit int64, joinedAt time.Time) ([]*Message, error)
	GetLastSequenceNumber(ctx context.Context, roomID string) (int64, error)
	SyncMessages(ctx context.Context, roomID string, lastSeq int64, limit int64, joinedAt time.Time) ([]*Message, error)
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

func (r *mongoRepository) SaveMany(ctx context.Context, msgs []*Message) error {
	if len(msgs) == 0 {
		return nil
	}

	docs := make([]any, len(msgs))
	for i, m := range msgs {
		docs[i] = m
	}

	opts := options.InsertMany().SetOrdered(false)
	_, err := r.col.InsertMany(ctx, docs, opts)

	var bwErr mongo.BulkWriteException
	if !errors.As(err, &bwErr) {
		return err
	}

	for _, we := range bwErr.WriteErrors {
		if we.Code != 11000 {
			return err
		}
	}
	return nil
}

func (r *mongoRepository) GetHistory(ctx context.Context, roomID string, limit int64, joinedAt time.Time) ([]*Message, error) {
	filter := bson.M{"roomId": roomID}

	if !joinedAt.IsZero() {
		filter["createdAt"] = bson.M{"$gte": joinedAt}
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "sequenceNumber", Value: -1}}).
		SetLimit(limit)

	cursor, err := r.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var messages []*Message
	if err := cursor.All(ctx, &messages); err != nil {
		return nil, err
	}

	return messages, nil
}

func (r *mongoRepository) GetLastSequenceNumber(ctx context.Context, roomID string) (int64, error) {
	filter := bson.M{"roomId": roomID}
	opts := options.FindOne().
		SetSort(bson.D{{Key: "sequenceNumber", Value: -1}}).
		SetProjection(bson.D{{Key: "sequenceNumber", Value: 1}, {Key: "_id", Value: 0}})

	var result struct {
		SequenceNumber int64 `bson:"sequenceNumber"`
	}
	err := r.col.FindOne(ctx, filter, opts).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	return result.SequenceNumber, nil
}

func (r *mongoRepository) SyncMessages(ctx context.Context, roomID string, lastSeq int64, limit int64, joinedAt time.Time) ([]*Message, error) {
	filter := bson.M{
		"roomId":         roomID,
		"sequenceNumber": bson.M{"$gt": lastSeq},
	}

	if !joinedAt.IsZero() {
		filter["createdAt"] = bson.M{"$gte": joinedAt}
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "sequenceNumber", Value: 1}}).
		SetLimit(limit)

	cursor, err := r.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var messages []*Message
	if err := cursor.All(ctx, &messages); err != nil {
		return nil, err
	}

	return messages, nil
}
