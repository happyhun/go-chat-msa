package apigateway

import (
	"fmt"

	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry   config.TelemetryConfig `mapstructure:"TELEMETRY"`
	Port        config.PortConfig      `mapstructure:"PORT"        validate:"required"`
	APIGateway  GatewayConfig          `mapstructure:"API_GATEWAY" validate:"required"`
	Registry    ServiceRegistry        `mapstructure:"REGISTRY"    validate:"required"`
	JWT         config.JWTConfig       `mapstructure:"JWT"         validate:"required"`
	Internal    config.InternalConfig  `mapstructure:"INTERNAL"    validate:"required"`
	UserService config.UserConfig      `mapstructure:"USER_SERVICE" validate:"required"`
}

type GatewayConfig struct {
	Server     config.HTTPServerConfig `mapstructure:"SERVER" validate:"required"`
	HTTPClient config.HTTPClientConfig `mapstructure:"HTTP_CLIENT" validate:"required"`
	GRPCClient config.GRPCClientConfig `mapstructure:"GRPC_CLIENT" validate:"required"`
	CORS       config.CORSConfig       `mapstructure:"CORS" validate:"required"`
	RateLimit  RateLimitConfig         `mapstructure:"RATE_LIMIT" validate:"required"`
}

type RateLimitConfig struct {
	Public        config.RateLimitConfig `mapstructure:"PUBLIC" validate:"required"`
	Authenticated config.RateLimitConfig `mapstructure:"AUTHENTICATED" validate:"required"`
}

type ServiceRegistry struct {
	UserService        config.HostConfig `mapstructure:"USER_SERVICE" validate:"required"`
	ChatService        config.HostConfig `mapstructure:"CHAT_SERVICE" validate:"required"`
	WSGateway          config.HostConfig `mapstructure:"WS_GATEWAY" validate:"required"`
	WebSocketEndpoints []string          `mapstructure:"WEBSOCKET_ENDPOINTS" validate:"required"`
}

func (c *Config) UserAddr() string {
	return fmt.Sprintf("%s:%s", c.Registry.UserService.Host, c.Port.UserGRPC)
}

func (c *Config) ChatAddr() string {
	return fmt.Sprintf("%s:%s", c.Registry.ChatService.Host, c.Port.ChatGRPC)
}

func (c *Config) WSGatewayAddr() string {
	return fmt.Sprintf("http://%s:%s", c.Registry.WSGateway.Host, c.Port.WSGateway)
}
