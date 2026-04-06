package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	pgTracer        = otel.Tracer("go-chat-msa/pgx")
	pgMeter         = otel.Meter("go-chat-msa/metrics/pg")
	pgQueryDuration metric.Float64Histogram
	pgQueryTotal    metric.Int64Counter
)

func init() {
	var err error
	pgQueryDuration, err = pgMeter.Float64Histogram("gochat_pg_query_duration_seconds",
		metric.WithDescription("PostgreSQL 쿼리 소요 시간"),
		metric.WithExplicitBucketBoundaries(.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pg_query_duration_seconds", "error", err)
	}
	pgQueryTotal, err = pgMeter.Int64Counter("gochat_pg_query",
		metric.WithDescription("PostgreSQL 쿼리 실행 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_pg_query", "error", err)
	}
}

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type instrumentedDBTX struct {
	inner dbtx
}

func InstrumentedDBTX(inner dbtx) dbtx {
	return &instrumentedDBTX{inner: inner}
}

func (d *instrumentedDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	op := sqlOp(sql)
	ctx, span := pgTracer.Start(ctx, "pg."+op, trace.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", op),
	))
	defer span.End()

	start := time.Now()
	tag, err := d.inner.Exec(ctx, sql, args...)
	pgQueryDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("operation", op)))
	pgQueryTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", op), attribute.String("status", pgStatus(err))))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
	}
	return tag, err
}

func (d *instrumentedDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	op := sqlOp(sql)
	ctx, span := pgTracer.Start(ctx, "pg."+op, trace.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", op),
	))
	defer span.End()

	start := time.Now()
	rows, err := d.inner.Query(ctx, sql, args...)
	pgQueryDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("operation", op)))
	pgQueryTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", op), attribute.String("status", pgStatus(err))))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
	}
	return rows, err
}

func (d *instrumentedDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	op := sqlOp(sql)
	ctx, span := pgTracer.Start(ctx, "pg."+op, trace.WithAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation", op),
	))
	defer span.End()

	start := time.Now()
	row := d.inner.QueryRow(ctx, sql, args...)
	pgQueryDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("operation", op)))
	pgQueryTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", op), attribute.String("status", "ok")))
	return row
}

func sqlOp(sql string) string {
	for line := range strings.SplitSeq(sql, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		if i := strings.IndexByte(line, ' '); i > 0 {
			return strings.ToUpper(line[:i])
		}
	}
	return "UNKNOWN"
}

func pgStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
