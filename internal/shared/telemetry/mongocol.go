package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	mongoTracer        = otel.Tracer("go-chat-msa/mongo")
	mongoMeter         = otel.Meter("go-chat-msa/metrics/mongo")
	mongoQueryDuration metric.Float64Histogram
	mongoQueryTotal    metric.Int64Counter
)

func init() {
	var err error
	mongoQueryDuration, err = mongoMeter.Float64Histogram("gochat_mongo_query_duration_seconds",
		metric.WithDescription("MongoDB 쿼리 소요 시간"),
		metric.WithExplicitBucketBoundaries(.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_query_duration_seconds", "error", err)
	}
	mongoQueryTotal, err = mongoMeter.Int64Counter("gochat_mongo_query",
		metric.WithDescription("MongoDB 쿼리 실행 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_mongo_query", "error", err)
	}
}

type InstrumentedCollection struct {
	inner *mongo.Collection
}

func NewInstrumentedCollection(col *mongo.Collection) *InstrumentedCollection {
	return &InstrumentedCollection{inner: col}
}

func (c *InstrumentedCollection) InsertMany(ctx context.Context, documents []any, opts ...*options.InsertManyOptions) (*mongo.InsertManyResult, error) {
	ctx, span := mongoTracer.Start(ctx, "mongo.InsertMany", trace.WithAttributes(
		attribute.String("db.system", "mongodb"),
		attribute.String("db.operation", "InsertMany"),
	))
	defer span.End()

	start := time.Now()
	result, err := c.inner.InsertMany(ctx, documents, opts...)
	mongoQueryDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("operation", "INSERT_MANY")))
	mongoQueryTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "INSERT_MANY"), attribute.String("status", mongoStatus(err))))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
	}
	return result, err
}

func (c *InstrumentedCollection) Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	ctx, span := mongoTracer.Start(ctx, "mongo.Find", trace.WithAttributes(
		attribute.String("db.system", "mongodb"),
		attribute.String("db.operation", "Find"),
	))
	defer span.End()

	start := time.Now()
	cursor, err := c.inner.Find(ctx, filter, opts...)
	mongoQueryDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("operation", "FIND")))
	mongoQueryTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "FIND"), attribute.String("status", mongoStatus(err))))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
	}
	return cursor, err
}

func (c *InstrumentedCollection) FindOne(ctx context.Context, filter any, opts ...*options.FindOneOptions) *mongo.SingleResult {
	ctx, span := mongoTracer.Start(ctx, "mongo.FindOne", trace.WithAttributes(
		attribute.String("db.system", "mongodb"),
		attribute.String("db.operation", "FindOne"),
	))
	defer span.End()

	start := time.Now()
	result := c.inner.FindOne(ctx, filter, opts...)
	mongoQueryDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("operation", "FIND_ONE")))
	mongoQueryTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "FIND_ONE"), attribute.String("status", "ok")))
	return result
}

func mongoStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
