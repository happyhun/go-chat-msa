package ops

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fixtureApp(t *testing.T) *app {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"api/openapi/openapi.yaml":       "openapi: 3.0.0",
		"db/migrations/postgres/001.sql": "SELECT 1;",
		"db/migrations/mongo/001.json":   "{}",
		"test/load/load.js":              "export default function() {}",
	}
	for _, group := range []string{"infra", "apps", "observability", "platform"} {
		files["deploy/helm/charts/"+group+"/Chart.yaml"] = "apiVersion: v2\nname: " + group + "\nversion: 1.0.0\ndependencies: []\n"
		files["deploy/helm/charts/"+group+"/Chart.lock"] = "dependencies: []\n"
	}
	for _, component := range []string{"prometheus", "grafana", "alloy", "loki", "tempo", "pyroscope"} {
		files["observability/"+component+"/config.yaml"] = "config: test"
	}
	for name, data := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	}
	return &app{root: root, cluster: "test-cluster", environment: "dev", helm: "helm", timeout: 5 * time.Minute, runner: &runner{dir: root, out: io.Discard, errOut: io.Discard}}
}

func fakeOutput(c invocation) ([]byte, error) {
	if c.name == "helm" && len(c.args) > 1 && c.args[0] == "dependency" && c.args[1] == "list" {
		return []byte("NAME VERSION REPOSITORY STATUS\nlocal 1.0.0 file://../local ok\n"), nil
	}
	if c.name == "helm" && c.args[0] == "template" && c.args[1] == "apps" {
		var result strings.Builder
		for _, service := range []string{"api-gateway", "websocket-service", "user-service", "chat-service", "frontend"} {
			fmt.Fprintf(&result, "---\nspec:\n  containers:\n  - image: go-chat-msa/%s:local\n", service)
		}
		return []byte(result.String()), nil
	}
	if c.name == "docker" && c.args[0] == "image" {
		return []byte("sha256:" + strings.Repeat("a", 64)), nil
	}
	if c.name == "kubectl" && strings.Contains(strings.Join(c.args, " "), "get deployment/postgres") {
		return nil, errors.New("not found")
	}
	return nil, nil
}

func TestDeploymentOrderAndImageTags(t *testing.T) {
	a := fixtureApp(t)
	var calls []invocation
	a.runner.execute = func(_ context.Context, c invocation) ([]byte, error) { calls = append(calls, c); return fakeOutput(c) }
	require.NoError(t, a.up(context.Background()))
	stage := a.stage
	defer a.cleanup()
	var upgrades, loads []string
	for _, c := range calls {
		if c.name == "helm" && c.args[0] == "upgrade" {
			upgrades = append(upgrades, c.args[2])
		}
		if c.name == "kind" {
			loads = append(loads, strings.Join(c.args, " "))
		}
	}
	require.Equal(t, []string{"infra", "observability", "migrations", "apps"}, upgrades)
	require.Len(t, loads, 5)
	for _, load := range loads {
		require.Contains(t, load, "--name test-cluster")
		require.Contains(t, load, ":sha256-"+strings.Repeat("a", 64))
	}
	for _, name := range []string{"images.yaml", "services/swagger-ui/files/openapi-spec/openapi.yaml", "migrations/files/postgres-migrations/001.sql", "migrations/files/mongo-migrations/001.json", "load/files/k6-load-scripts/load.js", "observability/files/grafana/config.yaml"} {
		require.FileExists(t, filepath.Join(stage, name))
	}
	a.cleanup()
	require.NoDirExists(t, stage)
}

func TestMigrationFailureStopsAppsAndCollectsDiagnostics(t *testing.T) {
	a := fixtureApp(t)
	defer a.cleanup()
	var calls []string
	failure := errors.New("migration failed")
	a.runner.execute = func(_ context.Context, c invocation) ([]byte, error) {
		call := c.name + " " + strings.Join(c.args, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "helm upgrade --install migrations ") {
			return nil, failure
		}
		return fakeOutput(c)
	}
	require.ErrorIs(t, a.up(context.Background()), failure)
	joined := strings.Join(calls, "\n")
	require.NotContains(t, joined, "helm upgrade --install apps ")
	require.Contains(t, joined, "describe job/postgres-migrate")
	require.Contains(t, joined, "logs job/mongo-migrate")
}

func TestDownOrderAndStopOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			a := fixtureApp(t)
			var releases []string
			deleted := false
			a.runner.execute = func(_ context.Context, c invocation) ([]byte, error) {
				if c.name == "helm" {
					releases = append(releases, c.args[1])
					if fail && c.args[1] == "apps" {
						return nil, errors.New("uninstall failed")
					}
				}
				if c.name == "kubectl" {
					deleted = true
					require.Contains(t, c.args, "go-chat-dev")
				}
				return nil, nil
			}
			err := a.down(context.Background())
			if fail {
				require.Error(t, err)
				require.Equal(t, []string{"load", "apps"}, releases)
				require.False(t, deleted)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"load", "apps", "migrations", "observability", "infra"}, releases)
				require.True(t, deleted)
			}
		})
	}
}

func TestDependencyCacheInvalidation(t *testing.T) {
	a := fixtureApp(t)
	builds := 0
	a.runner.execute = func(_ context.Context, c invocation) ([]byte, error) {
		if c.args[0] == "dependency" && c.args[1] == "build" {
			builds++
		}
		return fakeOutput(c)
	}
	ctx := context.Background()
	require.NoError(t, a.prepareDependencies(ctx, "infra"))
	require.NoError(t, a.prepareDependencies(ctx, "infra"))
	require.Equal(t, 1, builds)
	require.NoError(t, os.WriteFile(filepath.Join(a.helmDir(), "charts", "infra", "Chart.lock"), []byte("changed"), 0o600))
	require.NoError(t, a.prepareDependencies(ctx, "infra"))
	require.Equal(t, 2, builds)
}

func TestValidateEveryEnvironmentWithoutCluster(t *testing.T) {
	a := fixtureApp(t)
	defer a.cleanup()
	var lintCalls, renderCalls int
	a.runner.execute = func(_ context.Context, c invocation) ([]byte, error) {
		require.Equal(t, "helm", c.name)
		switch c.args[0] {
		case "lint":
			lintCalls++
		case "template":
			renderCalls++
		}
		return fakeOutput(c)
	}
	require.NoError(t, a.validate(context.Background()))
	require.Equal(t, 16, lintCalls)
	require.Equal(t, 16, renderCalls)
}

func TestDownloadChecksum(t *testing.T) {
	body := []byte("verified manifest")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	data, err := downloadVerified(context.Background(), server.URL, fmt.Sprintf("%x", sha256.Sum256(body)))
	require.NoError(t, err)
	require.Equal(t, body, data)
	_, err = downloadVerified(context.Background(), server.URL, "wrong")
	require.ErrorContains(t, err, "checksum mismatch")
}
