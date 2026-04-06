package websocket

import (
	"fmt"

	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry config.TelemetryConfig `mapstructure:"TELEMETRY"`
	Port      config.PortConfig      `mapstructure:"PORT"      validate:"required"`
	Registry  ServiceRegistry        `mapstructure:"REGISTRY"  validate:"required"`
	JWT       config.JWTConfig       `mapstructure:"JWT"       validate:"required"`
	WS        WebSocketConfig        `mapstructure:"WEBSOCKET" validate:"required"`
}

type WebSocketConfig struct {
	Server     config.HTTPWSServerConfig `mapstructure:"SERVER" validate:"required"`
	Manager    config.ManagerConfig      `mapstructure:"MANAGER" validate:"required"`
	GRPCClient config.GRPCClientConfig   `mapstructure:"GRPC_CLIENT" validate:"required"`
	RateLimit  RateLimitConfig           `mapstructure:"RATE_LIMIT" validate:"required"`
}

type RateLimitConfig struct {
	WSMessage config.RateLimitConfig `mapstructure:"WS_MESSAGE" validate:"required"`
}

type ServiceRegistry struct {
	UserService config.HostConfig `mapstructure:"USER_SERVICE" validate:"required"`
	ChatService config.HostConfig `mapstructure:"CHAT_SERVICE" validate:"required"`
}

func (c *Config) UserAddr() string {
	return fmt.Sprintf("%s:%s", c.Registry.UserService.Host, c.Port.UserGRPC)
}

func (c *Config) ChatAddr() string {
	return fmt.Sprintf("%s:%s", c.Registry.ChatService.Host, c.Port.ChatGRPC)
}
