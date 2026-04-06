package user

import (
	"go-chat-msa/internal/shared/config"
)

type Config struct {
	config.AppConfig `mapstructure:",squash"`
	Telemetry   config.TelemetryConfig `mapstructure:"TELEMETRY"`
	Port        config.PortConfig      `mapstructure:"PORT"         validate:"required"`
	DB          config.DBConfig        `mapstructure:"DB"           validate:"required"`
	JWT         config.JWTConfig       `mapstructure:"JWT"          validate:"required"`
	UserService config.UserConfig      `mapstructure:"USER_SERVICE" validate:"required"`
}
