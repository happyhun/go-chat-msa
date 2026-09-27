package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go-chat-msa/internal/ops"
)

func main() { os.Exit(run()) }

func run() int {
	terminated := errors.New("terminated")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case sig := <-signals:
			if sig == syscall.SIGTERM {
				cancel(terminated)
			} else {
				cancel(context.Canceled)
			}
		case <-ctx.Done():
		}
	}()
	err := ops.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "ops:", err)
	}
	if errors.Is(context.Cause(ctx), terminated) {
		return 143
	}
	if ctx.Err() != nil {
		return 130
	}
	if err != nil {
		return 1
	}
	return 0
}
