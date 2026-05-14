package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/logger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry        config.TelemetryConfig       `mapstructure:"TELEMETRY"`
	Port             config.PortConfig            `mapstructure:"PORT"             validate:"required"`
	DB               DBConfig                     `mapstructure:"DB"               validate:"required"`
	RetentionWorker  config.RetentionWorkerConfig `mapstructure:"RETENTION_WORKER" validate:"required"`
}

type DBConfig struct {
	PostgresURL string `mapstructure:"POSTGRES_URL" validate:"required"`
}

type Store interface {
	PurgeDeletedRooms(context.Context, pgtype.Timestamptz) (int64, error)
	PurgeDeletedUsers(context.Context, pgtype.Timestamptz) (int64, error)
}

type KindResult struct {
	Count int64
	Err   error
}

type Result struct {
	Threshold pgtype.Timestamptz
	Rooms     KindResult
	Users     KindResult
}

var retentionMeter = otel.Meter("go-chat-msa/retention")

var (
	retentionDuration    metric.Float64Histogram
	retentionPurgedTotal metric.Int64Counter
)

func init() {
	retentionDuration, _ = retentionMeter.Float64Histogram("gochat_retention_duration_seconds",
		metric.WithDescription("리텐션 퍼지 작업 소요 시간"),
		metric.WithExplicitBucketBoundaries(.1, .25, .5, 1, 2.5, 5, 10, 30),
	)
	retentionPurgedTotal, _ = retentionMeter.Int64Counter("gochat_retention_purged",
		metric.WithDescription("리텐션 퍼지 실행 횟수"),
	)
}

func LoadConfig() (*Config, error) {
	env := config.GetEnv()
	logger.InitLogger(env)

	return config.Load[Config]("configs", "base", env)
}

func PurgeDeleted(ctx context.Context, store Store, retentionDays int, now time.Time) (Result, error) {
	if retentionDays < 1 {
		return Result{}, fmt.Errorf("retention days must be positive: %d", retentionDays)
	}
	if now.IsZero() {
		now = time.Now()
	}

	threshold := pgtype.Timestamptz{
		Time:  now.AddDate(0, 0, -retentionDays),
		Valid: true,
	}
	result := Result{Threshold: threshold}

	rooms, roomsErr := store.PurgeDeletedRooms(ctx, threshold)
	result.Rooms = KindResult{Count: rooms, Err: roomsErr}

	users, usersErr := store.PurgeDeletedUsers(ctx, threshold)
	result.Users = KindResult{Count: users, Err: usersErr}

	return result, errors.Join(wrapKindError("rooms", roomsErr), wrapKindError("users", usersErr))
}

func RecordMetrics(ctx context.Context, result Result, duration time.Duration) {
	if !result.Threshold.Valid {
		return
	}

	retentionPurgedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", "rooms"),
		attribute.String("status", status(result.Rooms.Err)),
	))
	retentionPurgedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", "users"),
		attribute.String("status", status(result.Users.Err)),
	))
	retentionDuration.Record(ctx, duration.Seconds())
}

func LogResult(ctx context.Context, result Result) {
	if !result.Threshold.Valid {
		return
	}
	logKind(ctx, "rooms", result.Rooms)
	logKind(ctx, "users", result.Users)
}

func status(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func logKind(ctx context.Context, kind string, result KindResult) {
	if result.Err != nil {
		slog.ErrorContext(ctx, "failed to purge", "kind", kind, "error", result.Err)
		return
	}
	slog.InfoContext(ctx, "purged", "kind", kind, "count", result.Count)
}

func wrapKindError(kind string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("purge deleted %s: %w", kind, err)
}
