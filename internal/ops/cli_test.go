package ops

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfig(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		env       map[string]string
		wantError string
		check     func(*testing.T, config)
	}{
		{name: "dev defaults", args: []string{"dev-load"}, check: func(t *testing.T, c config) {
			require.Equal(t, 30*time.Minute, c.loadTimeout)
			require.Equal(t, 4, c.maxLogRequests)
			require.Equal(t, "kind-go-chat", c.kubeContext())
		}},
		{name: "qa defaults", args: []string{"qa-load"}, check: func(t *testing.T, c config) {
			require.Equal(t, 15*time.Minute, c.loadTimeout)
			require.Equal(t, 1, c.maxLogRequests)
		}},
		{name: "flags override environment", args: []string{"qa-load", "--cluster", "custom", "--timeout", "20m", "--follow-logs=false", "--max-log-requests", "2"}, env: map[string]string{
			"KIND_CLUSTER": "env-cluster", "K6_LOAD_TIMEOUT": "invalid", "K6_FOLLOW_LOGS": "invalid", "K6_MAX_LOG_REQUESTS": "invalid",
		}, check: func(t *testing.T, c config) {
			require.Equal(t, "kind-custom", c.kubeContext())
			require.Equal(t, 20*time.Minute, c.loadTimeout)
			require.False(t, c.followLogs)
			require.Equal(t, 2, c.maxLogRequests)
		}},
		{name: "environment values", args: []string{"dev-load"}, env: map[string]string{
			"K6_LOAD_TIMEOUT": "90", "K6_FOLLOW_LOGS": "true", "K6_WORKER_VUS": "100", "K6_PLATEAU_DURATION": "2m",
		}, check: func(t *testing.T, c config) {
			require.Equal(t, 90*time.Second, c.loadTimeout)
			require.True(t, c.followLogs)
			require.Equal(t, []string{"--set-string", "env.K6_WORKER_VUS=100", "--set-string", "env.K6_PLATEAU_DURATION=2m"}, c.loadValues)
		}},
		{name: "deployment timeout", args: []string{"test-up", "--timeout", "10m"}, check: func(t *testing.T, c config) { require.Equal(t, 10*time.Minute, c.timeout) }},
		{name: "unknown command", args: []string{"test-load"}, wantError: "unknown command"},
		{name: "private command", args: []string{"helm-deps"}, wantError: "unknown command"},
		{name: "unsupported option", args: []string{"helm-validate", "--cluster", "other"}, wantError: "flag provided but not defined"},
		{name: "extra argument", args: []string{"dev-up", "test-up"}, wantError: "unexpected arguments"},
		{name: "empty cluster", args: []string{"kind-delete", "--cluster="}, wantError: "cluster must not be empty"},
		{name: "zero timeout", args: []string{"dev-load", "--timeout", "0"}, wantError: "positive duration"},
		{name: "overflow timeout", args: []string{"dev-load", "--timeout", "999999999999999999999h"}, wantError: "positive duration"},
		{name: "zero workers", args: []string{"dev-load"}, env: map[string]string{"K6_WORKER_VUS": "0"}, wantError: "invalid K6_WORKER_VUS"},
		{name: "invalid log limit", args: []string{"dev-load", "--max-log-requests", "0"}, wantError: "positive integer"},
		{name: "invalid follow", args: []string{"dev-load"}, env: map[string]string{"K6_FOLLOW_LOGS": "yes"}, wantError: "invalid K6_FOLLOW_LOGS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseConfig(tt.args, func(key string) string { return tt.env[key] }, io.Discard)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestHelpNeedsNoToolsOrRepository(t *testing.T) {
	t.Setenv("PATH", "")
	t.Chdir(t.TempDir())
	for _, args := range [][]string{nil, {"--help"}, {"dev-load", "--help"}, {"helm-validate", "--help"}} {
		var out bytes.Buffer
		require.NoError(t, Run(context.Background(), args, &out, io.Discard))
		require.Contains(t, out.String(), "Usage:")
	}
}
