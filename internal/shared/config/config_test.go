package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type TestConfig struct {
	Env  string `mapstructure:"ENV" validate:"required"`
	Port struct {
		APIGateway string `mapstructure:"API_GATEWAY" validate:"required"`
	} `mapstructure:"PORT" validate:"required"`
}

func TestLoad(t *testing.T) {
	t.Parallel()
	configPath := "../../../configs"

	tests := []struct {
		name     string
		files    []string
		wantEnv  string
		wantPort string
	}{
		{
			name:     "Success: base 및 dev 설정 파일 정상 로드",
			files:    []string{"base", "dev"},
			wantEnv:  "dev",
			wantPort: "8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load[TestConfig](configPath, tt.files[0], tt.files[1])
			require.NoError(t, err)
			assert.Equal(t, tt.wantEnv, cfg.Env)
			assert.Equal(t, tt.wantPort, cfg.Port.APIGateway)
		})
	}
}

func TestLoad_ValidationFailure(t *testing.T) {
	t.Parallel()
	configPath := "../../../configs"

	tests := []struct {
		name       string
		errContain string
	}{
		{
			name:       "Failure: 필수 설정 누락 시 검증(Validation) 실패 처리",
			errContain: "config validation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			type InvalidConfig struct {
				NonExistent string `mapstructure:"NON_EXISTENT" validate:"required"`
			}
			_, err := Load[InvalidConfig](configPath, "base", "dev")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContain)
		})
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configPath string
		base       string
		override   string
		errContain string
	}{
		{
			name:       "Failure: 존재하지 않는 설정 파일 경로",
			configPath: "/nonexistent/path",
			base:       "base",
			override:   "",
			errContain: "failed to read base config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load[TestConfig](tt.configPath, tt.base, tt.override)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContain)
		})
	}
}
