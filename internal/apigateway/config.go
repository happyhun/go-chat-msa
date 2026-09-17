package apigateway

import (
	"fmt"
	"time"

	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry        config.TelemetryConfig `mapstructure:"TELEMETRY"`
	Port             config.PortConfig      `mapstructure:"PORT"        validate:"required"`
	APIGateway       GatewayConfig          `mapstructure:"API_GATEWAY" validate:"required"`
	Registry         ServiceRegistry        `mapstructure:"REGISTRY"    validate:"required"`
	JWT              config.JWTConfig       `mapstructure:"JWT"         validate:"required"`
	Internal         config.InternalConfig  `mapstructure:"INTERNAL"    validate:"required"`
	UserService      config.UserConfig      `mapstructure:"USER_SERVICE" validate:"required"`
	Redis            config.RedisConfig     `mapstructure:"REDIS"       validate:"required"`
	ChatService      config.ChatConfig      `mapstructure:"CHAT_SERVICE" validate:"required"`
}

type GatewayConfig struct {
	Server     config.HTTPServerConfig `mapstructure:"SERVER" validate:"required"`
	HTTPClient config.HTTPClientConfig `mapstructure:"HTTP_CLIENT" validate:"required"`
	GRPCClient config.GRPCClientConfig `mapstructure:"GRPC_CLIENT" validate:"required"`
	TicketTTL  time.Duration           `mapstructure:"TICKET_TTL" validate:"required"`
	RateLimit  RateLimitConfig         `mapstructure:"RATE_LIMIT" validate:"required"`
}

type RateLimitConfig struct {
	Public        config.RateLimitConfig `mapstructure:"PUBLIC" validate:"required"`
	Authenticated config.RateLimitConfig `mapstructure:"AUTHENTICATED" validate:"required"`
	WSTicket      config.RateLimitConfig `mapstructure:"WS_TICKET" validate:"required"`
}

type ServiceRegistry struct {
	UserService config.HostConfig `mapstructure:"USER_SERVICE" validate:"required"`
	ChatService config.HostConfig `mapstructure:"CHAT_SERVICE" validate:"required"`
	WebSocket   config.HostConfig `mapstructure:"WEBSOCKET" validate:"required"`
}

func (c *Config) UserAddr() string {
	return fmt.Sprintf("dns:///%s:%s", c.Registry.UserService.Host, c.Port.UserGRPC)
}

func (c *Config) ChatAddr() string {
	return fmt.Sprintf("dns:///%s:%s", c.Registry.ChatService.Host, c.Port.ChatGRPC)
}

func (c *Config) WebSocketAddr() string {
	return fmt.Sprintf("http://%s:%s", c.Registry.WebSocket.Host, c.Port.WebSocket)
}
