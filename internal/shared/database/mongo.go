package database

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func NewMongo(uri string, opts ...MongoOption) (*mongo.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()

	clientOptions := options.Client().ApplyURI(uri)

	for _, o := range opts {
		o(clientOptions)
	}

	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to mongo: %w", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("unable to ping mongo: %w", err)
	}

	return client, nil
}

type MongoOption func(*options.ClientOptions)

func WithPoolMonitor(pm *event.PoolMonitor) MongoOption {
	return func(o *options.ClientOptions) {
		o.SetPoolMonitor(pm)
	}
}
