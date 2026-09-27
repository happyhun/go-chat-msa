package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkerProgress(t *testing.T) {
	var job jobStatus
	require.NoError(t, json.Unmarshal([]byte(`{"spec":{"parallelism":4,"completions":4}}`), &job))
	var pods podList
	require.NoError(t, json.Unmarshal([]byte(`{"items":[
  {"metadata":{"name":"running"},"status":{"phase":"Running","containerStatuses":[{"name":"k6","state":{"running":{"startedAt":"now"}}}]}},
  {"metadata":{"name":"done"},"status":{"containerStatuses":[{"name":"k6","state":{"terminated":{"exitCode":0,"reason":"Completed"}}}]}},
  {"metadata":{"name":"failed"},"status":{"containerStatuses":[{"name":"k6","state":{"terminated":{"exitCode":99,"reason":"Error"}}}]}},
  {"metadata":{"name":"pending"},"status":{"phase":"Pending","containerStatuses":[{"name":"sidecar","state":{"running":{"startedAt":"now"}}},{"name":"k6","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}
 ]}`), &pods))
	phase, detail, ready := workerProgress(pods, job)
	require.Equal(t, "Preparing", phase)
	require.False(t, ready)
	require.Contains(t, detail, "1 running, 1 succeeded, 1 failed / 4")
	require.Contains(t, detail, "failed:Error(exit=99)")
	require.Contains(t, detail, "pending:ImagePullBackOff")
}

func TestJobMonitorTerminalStatesWithoutPodDetails(t *testing.T) {
	for _, state := range []string{"Complete", "Failed"} {
		t.Run(state, func(t *testing.T) {
			a := fixtureApp(t)
			a.loadTimeout = time.Second
			a.runner.execute = func(_ context.Context, c invocation) ([]byte, error) {
				if strings.Contains(strings.Join(c.args, " "), "get job/") {
					return []byte(`{"status":{"conditions":[{"type":"` + state + `","status":"True","reason":"ThresholdsFailed","message":"threshold exceeded"}]}}`), nil
				}
				return nil, errors.New("pod details unavailable")
			}
			err := a.monitorJob(context.Background(), "k6-c10k")
			if state == "Complete" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "ThresholdsFailed threshold exceeded")
			}
		})
	}
}

func TestJobMonitorRetriesAndTimesOut(t *testing.T) {
	a := fixtureApp(t)
	a.loadTimeout = 20 * time.Millisecond
	var output bytes.Buffer
	a.runner.out = &output
	a.runner.execute = func(_ context.Context, _ invocation) ([]byte, error) { return nil, errors.New("API unavailable") }
	err := a.monitorJob(context.Background(), "k6-c10k")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, output.String(), "Retrying")
	require.Contains(t, output.String(), "Timed out")
}

func TestJobMonitorCancellation(t *testing.T) {
	a := fixtureApp(t)
	a.loadTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	a.runner.execute = func(_ context.Context, _ invocation) ([]byte, error) { cancel(); return nil, context.Canceled }
	require.ErrorIs(t, a.monitorJob(ctx, "k6-c10k"), context.Canceled)
}

func TestHPARestoredAfterCancellation(t *testing.T) {
	a := fixtureApp(t)
	a.environment = "qa"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restored := false
	a.runner.execute = func(ctx context.Context, c invocation) ([]byte, error) {
		call := strings.Join(c.args, " ")
		switch {
		case c.name == "helm":
			return []byte("kind: HorizontalPodAutoscaler\n"), nil
		case strings.Contains(call, "scale deployment/"):
			cancel()
			return nil, context.Canceled
		case strings.Contains(call, "apply -f -"):
			require.NoError(t, ctx.Err())
			_, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			require.Equal(t, "kind: HorizontalPodAutoscaler\n", string(c.input))
			restored = true
		}
		return nil, nil
	}
	require.ErrorIs(t, a.resetHPA(ctx), context.Canceled)
	require.True(t, restored)
}

func TestHPARestoreFailureIsReported(t *testing.T) {
	a := fixtureApp(t)
	a.environment = "qa"
	a.runner.execute = func(_ context.Context, c invocation) ([]byte, error) {
		if c.name == "helm" {
			return []byte("kind: HorizontalPodAutoscaler\n"), nil
		}
		call := strings.Join(c.args, " ")
		if strings.Contains(call, "scale deployment/") {
			return nil, errors.New("scale failed")
		}
		if strings.Contains(call, "apply -f -") {
			return nil, errors.New("apply failed")
		}
		return nil, nil
	}
	err := a.resetHPA(context.Background())
	require.ErrorContains(t, err, "scale failed")
	require.ErrorContains(t, err, "restore WebSocket HPA: apply failed")
}

func TestLoadSummary(t *testing.T) {
	summary, found := resultSummary("noise\n  █ THRESHOLDS\n    checks: pass\n  █ TOTAL RESULTS\n    http: 42\nrunning (1m), 0 VUs\n  ignored\n")
	require.True(t, found)
	require.Contains(t, summary, "checks: pass")
	require.Contains(t, summary, "http: 42")
	require.NotContains(t, summary, "noise")
	require.NotContains(t, summary, "ignored")
	_, found = resultSummary("unrelated log\n")
	require.False(t, found)
}
