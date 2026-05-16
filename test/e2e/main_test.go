//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

type E2ESuite struct {
	suite.Suite
	namespace      string
	gatewayBaseURL string
	wsBaseURL      string
}

func TestE2ESuite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}
	suite.Run(t, new(E2ESuite))
}

func (s *E2ESuite) SetupSuite() {
	setupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	s.startKubernetes(setupCtx)
}

func (s *E2ESuite) TearDownTest() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.cleanupDatabases(cleanupCtx)
}

func (s *E2ESuite) startKubernetes(ctx context.Context) {
	s.namespace = getenvDefault("E2E_K8S_NAMESPACE", "go-chat-test")
	s.gatewayBaseURL = strings.TrimRight(getenvDefault("E2E_GATEWAY_BASE_URL", "http://localhost:30080/api"), "/")
	s.wsBaseURL = strings.TrimRight(getenvDefault("E2E_WS_BASE_URL", "http://localhost:30080/ws-api"), "/")

	s.Require().NoError(s.runKubectl(ctx, "get", "namespace", s.namespace))
	for _, deployment := range []string{
		"postgres", "mongo", "redis",
		"prometheus", "loki", "tempo", "pyroscope", "alloy", "grafana",
		"user-service", "chat-service", "api-gateway", "websocket-service", "ws-gateway", "frontend",
	} {
		s.Require().NoError(s.runKubectl(ctx, "-n", s.namespace, "rollout", "status", "deployment/"+deployment, "--timeout=180s"))
	}
}

func getenvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func (s *E2ESuite) runKubectl(ctx context.Context, args ...string) error {
	_, err := s.kubectlOutput(ctx, args...)
	return err
}

func (s *E2ESuite) kubectlOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl %s failed: %w\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *E2ESuite) cleanupKubernetesDatabases(ctx context.Context) {
	if err := s.runKubectl(ctx, "-n", s.namespace, "exec", "deployment/postgres", "--",
		"psql", "-U", "postgres", "-d", "go_chat", "-c",
		"TRUNCATE TABLE users, rooms, room_members RESTART IDENTITY CASCADE;",
	); err != nil {
		s.T().Logf("cleanup: k8s postgres truncate error: %v", err)
	}

	if err := s.runKubectl(ctx, "-n", s.namespace, "exec", "deployment/mongo", "--",
		"mongosh", "chat_service", "--quiet", "--eval", "db.dropDatabase()",
	); err != nil {
		s.T().Logf("cleanup: k8s mongo drop error: %v", err)
	}
}

func (s *E2ESuite) requireDeploymentReadyReplicas(ctx context.Context, name string, expected int) {
	out, err := s.kubectlOutput(ctx, "-n", s.namespace, "get", "deployment", name, "-o", "jsonpath={.status.readyReplicas}")
	s.Require().NoError(err)
	ready, err := strconv.Atoi(strings.TrimSpace(out))
	s.Require().NoError(err, "ready replicas must be numeric for deployment/%s: %q", name, out)
	s.Require().Equal(expected, ready, "deployment/%s ready replicas", name)
}

func (s *E2ESuite) redisKeys(ctx context.Context, pattern string) []string {
	out, err := s.kubectlOutput(ctx, "-n", s.namespace, "exec", "deployment/redis", "--",
		"redis-cli", "--raw", "KEYS", pattern,
	)
	s.Require().NoError(err)
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Fields(out)
}
