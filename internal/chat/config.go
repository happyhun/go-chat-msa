package chat

import (
	"fmt"
	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry        config.TelemetryConfig   `mapstructure:"TELEMETRY"`
	Port             config.PortConfig        `mapstructure:"PORT"         validate:"required"`
	DB               config.DBConfig          `mapstructure:"DB"           validate:"required"`
	ChatService      config.ChatConfig        `mapstructure:"CHAT_SERVICE" validate:"required"`
	Persistence      config.PersistenceConfig `mapstructure:"PERSISTENCE" validate:"required"`
	Registry         ServiceRegistry          `mapstructure:"REGISTRY" validate:"required"`
}

type ServiceRegistry struct {
	UserService config.HostConfig `mapstructure:"USER_SERVICE" validate:"required"`
	NATS        config.HostConfig `mapstructure:"NATS" validate:"required"`
}

func (c *Config) UserAddr() string {
	return fmt.Sprintf("dns:///%s:%s", c.Registry.UserService.Host, c.Port.UserGRPC)
}

func (c *Config) NATSURL() string {
	return fmt.Sprintf("nats://%s:%s", c.Registry.NATS.Host, c.Port.NATS)
}
