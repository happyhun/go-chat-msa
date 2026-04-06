package logger

import (
	"log/slog"
	"os"
)

func InitLogger(env string) *slog.Logger {
	var logLevel slog.Level
	if env == "prod" {
		logLevel = slog.LevelInfo
	} else {
		logLevel = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{
		Level:     logLevel,
		AddSource: true,
	}

	jsonHandler := slog.NewJSONHandler(os.Stdout, opts)
	l := slog.New(NewTraceHandler(jsonHandler))

	slog.SetDefault(l)

	return l
}
