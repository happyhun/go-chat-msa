package chat

import (
	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry   config.TelemetryConfig `mapstructure:"TELEMETRY"`
	Port        config.PortConfig      `mapstructure:"PORT"         validate:"required"`
	DB          config.DBConfig        `mapstructure:"DB"           validate:"required"`
	ChatService config.ChatConfig      `mapstructure:"CHAT_SERVICE" validate:"required"`
}
