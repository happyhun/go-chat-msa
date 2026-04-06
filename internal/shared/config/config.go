package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
)

type AppConfig struct {
	Env             string        `mapstructure:"ENV" validate:"required"`
	ShutdownTimeout time.Duration `mapstructure:"SHUTDOWN_TIMEOUT" validate:"required"`
}

type TelemetryConfig struct {
	OTelEndpoint      string `mapstructure:"OTEL_ENDPOINT"`
	PyroscopeEndpoint string `mapstructure:"PYROSCOPE_ENDPOINT"`
}

type PortConfig struct {
	APIGateway  string `mapstructure:"API_GATEWAY"  validate:"required"`
	WSGateway   string `mapstructure:"WS_GATEWAY"   validate:"required"`
	WebSocket   string `mapstructure:"WEBSOCKET"    validate:"required"`
	UserGRPC   string `mapstructure:"USER_GRPC"   validate:"required"`
	ChatGRPC   string `mapstructure:"CHAT_GRPC"   validate:"required"`
}

type HTTPServerConfig struct {
	ReadTimeout  time.Duration `mapstructure:"READ_TIMEOUT" validate:"required"`
	WriteTimeout time.Duration `mapstructure:"WRITE_TIMEOUT" validate:"required"`
	IdleTimeout  time.Duration `mapstructure:"IDLE_TIMEOUT" validate:"required"`
}

type GRPCClientConfig struct {
	Timeout   time.Duration             `mapstructure:"TIMEOUT" validate:"required"`
	Keepalive GRPCClientKeepaliveConfig `mapstructure:"KEEPALIVE" validate:"required"`
}

type GRPCServerConfig struct {
	Timeout   time.Duration             `mapstructure:"TIMEOUT" validate:"required"`
	Keepalive GRPCServerKeepaliveConfig `mapstructure:"KEEPALIVE" validate:"required"`
}

type GRPCClientKeepaliveConfig struct {
	Time    time.Duration `mapstructure:"TIME" validate:"required"`
	Timeout time.Duration `mapstructure:"TIMEOUT" validate:"required"`
}

type GRPCServerKeepaliveConfig struct {
	Time    time.Duration `mapstructure:"TIME" validate:"required"`
	Timeout time.Duration `mapstructure:"TIMEOUT" validate:"required"`
	MinTime time.Duration `mapstructure:"MIN_TIME" validate:"required"`
}

type HTTPClientConfig struct {
	Timeout             time.Duration `mapstructure:"TIMEOUT" validate:"required"`
	MaxIdleConns        int           `mapstructure:"MAX_IDLE_CONNS" validate:"required"`
	MaxIdleConnsPerHost int           `mapstructure:"MAX_IDLE_CONNS_PER_HOST" validate:"required"`
}

type HTTPWSServerConfig struct {
	ReadHeaderTimeout time.Duration `mapstructure:"READ_HEADER_TIMEOUT" validate:"required"`
}

type HostConfig struct {
	Host string `mapstructure:"HOST" validate:"required"`
}

type JWTConfig struct {
	Secret string `mapstructure:"SECRET" validate:"required"`
}

type CORSConfig struct {
	AllowedOrigins []string `mapstructure:"ALLOWED_ORIGINS" validate:"required,min=1"`
}

type InternalConfig struct {
	Secret string `mapstructure:"SECRET" validate:"required"`
}

type DBConfig struct {
	PostgresURL string `mapstructure:"POSTGRES_URL" validate:"required"`
	MongoURI    string `mapstructure:"MONGO_URI" validate:"required"`
}

type ManagerConfig struct {
	WriteWait   time.Duration `mapstructure:"WRITE_WAIT" validate:"required"`
	PongWait    time.Duration `mapstructure:"PONG_WAIT" validate:"required"`
	PingPeriod  time.Duration `mapstructure:"PING_PERIOD" validate:"required"`
	IdleTimeout time.Duration `mapstructure:"IDLE_TIMEOUT" validate:"required"`
	MaxLength   int           `mapstructure:"MAX_LENGTH" validate:"required"`
}

type UserConfig struct {
	Validation ValidationConfig `mapstructure:"VALIDATION" validate:"required"`
	Token      TokenConfig      `mapstructure:"TOKEN" validate:"required"`
	Room       RoomConfig       `mapstructure:"ROOM" validate:"required"`
	Search     SearchConfig     `mapstructure:"SEARCH" validate:"required"`
	GRPCServer GRPCServerConfig `mapstructure:"GRPC_SERVER" validate:"required"`
}

type ValidationConfig struct {
	MinUsernameLength int `mapstructure:"MIN_USERNAME_LENGTH" validate:"required"`
	MaxUsernameLength int `mapstructure:"MAX_USERNAME_LENGTH" validate:"required"`
	MinPasswordLength int `mapstructure:"MIN_PASSWORD_LENGTH" validate:"required"`
	MaxPasswordLength int `mapstructure:"MAX_PASSWORD_LENGTH" validate:"required"`
}

type TokenConfig struct {
	AccessTokenExpirationMinutes int           `mapstructure:"ACCESS_TOKEN_EXPIRATION_MINUTES" validate:"required"`
	RefreshTokenExpirationDays   int           `mapstructure:"REFRESH_TOKEN_EXPIRATION_DAYS" validate:"required"`
	TokenPurgeInterval           time.Duration `mapstructure:"TOKEN_PURGE_INTERVAL" validate:"required"`
}

type RoomConfig struct {
	MaxCapacity   int32 `mapstructure:"MAX_CAPACITY" validate:"required"`
	MaxNameLength int   `mapstructure:"MAX_NAME_LENGTH" validate:"required"`
}

type SearchConfig struct {
	DefaultLimit int32 `mapstructure:"DEFAULT_LIMIT" validate:"required"`
	MaxLimit     int32 `mapstructure:"MAX_LIMIT" validate:"required"`
}

type ChatConfig struct {
	Message    MessageConfig    `mapstructure:"MESSAGE" validate:"required"`
	History    HistoryConfig    `mapstructure:"HISTORY" validate:"required"`
	Sync       SyncConfig       `mapstructure:"SYNC" validate:"required"`
	GRPCServer GRPCServerConfig `mapstructure:"GRPC_SERVER" validate:"required"`
}

type MessageConfig struct {
	MaxLength int `mapstructure:"MAX_LENGTH" validate:"required"`
}

type HistoryConfig struct {
	DefaultLimit int64 `mapstructure:"DEFAULT_LIMIT" validate:"required"`
	MaxLimit     int64 `mapstructure:"MAX_LIMIT" validate:"required"`
}

type SyncConfig struct {
	DefaultLimit int64 `mapstructure:"DEFAULT_LIMIT" validate:"required"`
	MaxLimit     int64 `mapstructure:"MAX_LIMIT" validate:"required"`
}

type RetentionWorkerConfig struct {
	Schedule      string        `mapstructure:"SCHEDULE"       validate:"required"`
	RetentionDays int           `mapstructure:"RETENTION_DAYS" validate:"required,min=1"`
	JobTimeout    time.Duration `mapstructure:"JOB_TIMEOUT"    validate:"required"`
}

type RateLimitConfig struct {
	RPS   float64       `mapstructure:"RPS"   validate:"required,gt=0"`
	Burst int           `mapstructure:"BURST" validate:"required,min=1"`
	TTL   time.Duration `mapstructure:"TTL"   validate:"required,gt=0"`
}

func GetEnv() string {
	env := os.Getenv("APP_ENV")
	if env == "" {
		return "dev"
	}
	return env
}

func Load[T any](configPath, baseName, overrideName string) (*T, error) {
	v := viper.New()

	v.SetConfigName(baseName)
	v.SetConfigType("yaml")
	v.AddConfigPath(configPath)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read base config (%s): %w", baseName, err)
	}

	if overrideName != "" {
		v.SetConfigName(overrideName)
		if err := v.MergeInConfig(); err != nil {
			return nil, fmt.Errorf("failed to merge override config (%s): %w", overrideName, err)
		}
	}

	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	var cfg T
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	validate := validator.New()
	if err := validate.Struct(&cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}
