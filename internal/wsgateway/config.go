package wsgateway

import (
	"time"

	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry config.TelemetryConfig `mapstructure:"TELEMETRY"`
	Port      config.PortConfig      `mapstructure:"PORT"       validate:"required"`
	WSGateway WSGatewayConfig        `mapstructure:"WS_GATEWAY" validate:"required"`
	Registry  ServiceRegistry        `mapstructure:"REGISTRY"   validate:"required"`
	JWT       config.JWTConfig       `mapstructure:"JWT"        validate:"required"`
	Internal  config.InternalConfig  `mapstructure:"INTERNAL"   validate:"required"`
}

type WSGatewayConfig struct {
	Server     config.HTTPWSServerConfig `mapstructure:"SERVER" validate:"required"`
	HTTPClient config.HTTPClientConfig   `mapstructure:"HTTP_CLIENT" validate:"required"`
	CORS       config.CORSConfig         `mapstructure:"CORS" validate:"required"`
	TicketTTL  time.Duration             `mapstructure:"TICKET_TTL" validate:"required"`
	RateLimit  RateLimitConfig           `mapstructure:"RATE_LIMIT" validate:"required"`
}

type RateLimitConfig struct {
	Public      config.RateLimitConfig `mapstructure:"PUBLIC" validate:"required"`
	WSEstablish config.RateLimitConfig `mapstructure:"WS_ESTABLISH" validate:"required"`
}

type ServiceRegistry struct {
	WebSocketEndpoints []string `mapstructure:"WEBSOCKET_ENDPOINTS" validate:"required"`
}
