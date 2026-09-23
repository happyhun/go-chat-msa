package main

import (
	"context"
	"net"
	"testing"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/user"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

func TestRunServerShutdownWithActiveHealthWatch(t *testing.T) {
	server := grpc.NewServer()
	t.Cleanup(server.Stop)
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	listener := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = listener.Close() })
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	conn, err := grpc.NewClient("passthrough:///test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	watchCtx, cancelWatch := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWatch()
	watch, err := grpc_health_v1.NewHealthClient(conn).Watch(watchCtx, &grpc_health_v1.HealthCheckRequest{})
	require.NoError(t, err)
	_, err = watch.Recv()
	require.NoError(t, err)

	cfg := &user.Config{}
	cfg.Port = config.PortConfig{UserGRPC: "0"}
	cfg.ShutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, cfg, server, healthServer) }()
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked by an active health watch")
	}
	require.NoError(t, <-serveDone)
}
