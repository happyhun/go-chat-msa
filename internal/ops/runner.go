package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type invocation struct {
	name    string
	args    []string
	input   []byte
	capture bool
}

type runner struct {
	dir     string
	out     io.Writer
	errOut  io.Writer
	execute func(context.Context, invocation) ([]byte, error)
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func (r *runner) command(ctx context.Context, c invocation) *exec.Cmd {
	// #nosec G204 -- Tool names and arguments come from the local CLI, without a shell.
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	cmd.Dir = r.dir
	cmd.Stdin = bytes.NewReader(c.input)
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func (r *runner) call(ctx context.Context, c invocation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.execute != nil {
		return r.execute(ctx, c)
	}
	cmd := r.command(ctx, c)
	var stdout, stderr bytes.Buffer
	if c.capture {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s %s: %w%s", c.name, strings.Join(c.args, " "), err, errorDetail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func errorDetail(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return ": " + s
	}
	return ""
}

func (r *runner) run(ctx context.Context, name string, args ...string) error {
	_, err := r.call(ctx, invocation{name: name, args: args})
	return err
}

func (r *runner) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.call(ctx, invocation{name: name, args: args, capture: true})
}

type process struct {
	cancel context.CancelFunc
	done   chan error
}

func (r *runner) start(ctx context.Context, name string, args ...string) (*process, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := r.command(ctx, invocation{name: name, args: args})
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	p := &process{cancel: cancel, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait(); close(p.done) }()
	return p, nil
}

func (p *process) stop() {
	if p != nil {
		p.cancel()
		<-p.done
	}
}
