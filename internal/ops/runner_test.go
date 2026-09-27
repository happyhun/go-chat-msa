package ops

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunnerHelper(_ *testing.T) {
	if os.Getenv("OPS_TEST_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		switch os.Args[i+1] {
		case "echo":
			_, _ = fmt.Fprint(os.Stdout, strings.Join(os.Args[i+2:], "|"))
		case "wait":
			time.Sleep(time.Minute)
		case "fail":
			_, _ = fmt.Fprint(os.Stderr, "diagnostic detail")
			os.Exit(7)
		}
		os.Exit(0)
	}
}

func TestRunnerArgumentsAndErrors(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("OPS_TEST_HELPER", "1")
	r := &runner{dir: t.TempDir(), out: io.Discard, errOut: io.Discard}
	data, err := r.output(context.Background(), executable, "-test.run=^TestRunnerHelper$", "--", "echo", "with spaces", "$(touch should-not-exist)")
	require.NoError(t, err)
	require.Equal(t, "with spaces|$(touch should-not-exist)", string(data))
	_, err = r.output(context.Background(), executable, "-test.run=^TestRunnerHelper$", "--", "fail")
	require.ErrorContains(t, err, "exit status 7")
	require.ErrorContains(t, err, "diagnostic detail")
}

func TestRunnerCancellationAndLogStop(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("OPS_TEST_HELPER", "1")
	r := &runner{dir: t.TempDir(), out: io.Discard, errOut: io.Discard}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = r.run(ctx, executable, "-test.run=^TestRunnerHelper$", "--", "wait")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	p, err := r.start(context.Background(), executable, "-test.run=^TestRunnerHelper$", "--", "wait")
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { p.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("background log process was not reaped")
	}
	p.stop()
}
