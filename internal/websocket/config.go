package websocket

import (
	"fmt"
	"time"

	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry        config.TelemetryConfig `mapstructure:"TELEMETRY"`
	Port             config.PortConfig      `mapstructure:"PORT"      validate:"required"`
	Registry         ServiceRegistry        `mapstructure:"REGISTRY"  validate:"required"`
	Internal         config.InternalConfig  `mapstructure:"INTERNAL"  validate:"required"`
	Redis            config.RedisConfig     `mapstructure:"REDIS"     validate:"required"`
	WS               WebSocketConfig        `mapstructure:"WEBSOCKET" validate:"required"`
}

type WebSocketConfig struct {
	Server     config.HTTPWSServerConfig `mapstructure:"SERVER" validate:"required"`
	Manager    config.ManagerConfig      `mapstructure:"MANAGER" validate:"required"`
	GRPCClient config.GRPCClientConfig   `mapstructure:"GRPC_CLIENT" validate:"required"`
	RateLimit  RateLimitConfig           `mapstructure:"RATE_LIMIT" validate:"required"`
	NATS       NATSConfig                `mapstructure:"NATS" validate:"required"`

	AllowedOrigins []string `mapstructure:"ALLOWED_ORIGINS"`
}

type NATSConfig struct {
	FlushTimeout    time.Duration `mapstructure:"FLUSH_TIMEOUT"     validate:"required,gt=0"`
	MaxDeliveryLag  time.Duration `mapstructure:"MAX_DELIVERY_LAG"  validate:"required,gt=0"`
	SubPendingMsgs  int           `mapstructure:"SUB_PENDING_MSGS"  validate:"required,min=1"`
	SubPendingBytes int           `mapstructure:"SUB_PENDING_BYTES" validate:"required,min=1"`
}

type RateLimitConfig struct {
	WSMessage config.RateLimitConfig `mapstructure:"WS_MESSAGE" validate:"required"`
	WSConnect config.RateLimitConfig `mapstructure:"WS_CONNECT" validate:"required"`
}

type ServiceRegistry struct {
	UserService config.HostConfig `mapstructure:"USER_SERVICE" validate:"required"`
	NATS        config.HostConfig `mapstructure:"NATS"         validate:"required"`
}

func (c *Config) UserAddr() string {
	return fmt.Sprintf("dns:///%s:%s", c.Registry.UserService.Host, c.Port.UserGRPC)
}

func (c *Config) NATSURL() string {
	return fmt.Sprintf("nats://%s:%s", c.Registry.NATS.Host, c.Port.NATS)
}
