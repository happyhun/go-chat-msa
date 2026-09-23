package config

import (
	"math"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/require"
)

func TestPaginationConfig(t *testing.T) {
	validate := validator.New()
	for _, tt := range []struct {
		name         string
		defaultLimit int32
		maxLimit     int32
		valid        bool
	}{
		{"valid", 20, 100, true},
		{"equal", 100, 100, true},
		{"negative default", -1, 100, false},
		{"negative maximum", 20, -1, false},
		{"zero default", 0, 100, false},
		{"zero maximum", 20, 0, false},
		{"default exceeds maximum", 101, 100, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, cfg := range []any{
				SearchConfig{DefaultLimit: tt.defaultLimit, MaxLimit: tt.maxLimit},
				HistoryConfig{DefaultLimit: int64(tt.defaultLimit), MaxLimit: int64(tt.maxLimit)},
				SyncConfig{DefaultLimit: int64(tt.defaultLimit), MaxLimit: int64(tt.maxLimit)},
			} {
				err := validate.Struct(cfg)
				if tt.valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
		})
	}
	require.Error(t, validate.Struct(HistoryConfig{DefaultLimit: 1, MaxLimit: math.MaxInt64}))
	require.Error(t, validate.Struct(SyncConfig{DefaultLimit: 1, MaxLimit: math.MaxInt64}))
}

func TestManagerTimingConfig(t *testing.T) {
	validate := validator.New()
	valid := ManagerConfig{WriteWait: time.Second, PongWait: time.Minute, PingPeriod: 30 * time.Second, IdleTimeout: time.Minute, MaxLength: 100}
	require.NoError(t, validate.Struct(valid))
	for _, field := range []string{"write", "pong", "ping", "idle", "equal ping", "late ping"} {
		t.Run(field, func(t *testing.T) {
			cfg := valid
			switch field {
			case "write":
				cfg.WriteWait = -time.Second
			case "pong":
				cfg.PongWait = -time.Second
			case "ping":
				cfg.PingPeriod = -time.Second
			case "idle":
				cfg.IdleTimeout = -time.Second
			case "equal ping":
				cfg.PingPeriod = cfg.PongWait
			case "late ping":
				cfg.PingPeriod = cfg.PongWait + time.Second
			}
			require.Error(t, validate.Struct(cfg))
		})
	}
}
