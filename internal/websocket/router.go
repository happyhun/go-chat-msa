package websocket

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/websocket/hub"
	"go-chat-msa/internal/websocket/roomlease"
	"go-chat-msa/internal/websocket/roomseq"
	"go-chat-msa/internal/wsgateway/loadbalance"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const (
	wsReadBufferSize           = 4096
	wsWriteBufferSize          = 4096
	readinessTimeout           = 2 * time.Second
	persistQueueReadyThreshold = 0.80
)

type RouterOption func(*routerOptions)

type routerOptions struct {
	shutdownTimeout time.Duration
	redisClient     *redis.Client
	roomLeaseStore  *roomlease.Store
	sequenceFloor   *roomseq.Store
	chatHealth      grpc_health_v1.HealthClient
	userHealth      grpc_health_v1.HealthClient
}

func WithShutdownTimeout(timeout time.Duration) RouterOption {
	return func(o *routerOptions) {
		o.shutdownTimeout = timeout
	}
}

func WithRedisClient(client *redis.Client) RouterOption {
	return func(o *routerOptions) {
		o.redisClient = client
	}
}

func WithRoomLeaseStore(store *roomlease.Store) RouterOption {
	return func(o *routerOptions) {
		o.roomLeaseStore = store
	}
}

func WithSequenceFloorStore(store *roomseq.Store) RouterOption {
	return func(o *routerOptions) {
		o.sequenceFloor = store
	}
}

func WithHealthClients(chatHealth, userHealth grpc_health_v1.HealthClient) RouterOption {
	return func(o *routerOptions) {
		o.chatHealth = chatHealth
		o.userHealth = userHealth
	}
}

type Router struct {
	mux      *http.ServeMux
	upgrader websocket.Upgrader

	advertisedAddr string
	hashRing       *loadbalance.HashRing

	userClient  userpb.UserServiceClient
	chatHealth  grpc_health_v1.HealthClient
	userHealth  grpc_health_v1.HealthClient
	redisClient *redis.Client
	manager     *hub.Manager
	store       *chatStoreAdapter
}

func NewRouter(
	chatClient chatpb.ChatServiceClient,
	userClient userpb.UserServiceClient,
	cfg WebSocketConfig,
	hashRing *loadbalance.HashRing,
	opts ...RouterOption,
) *Router {
	options := routerOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	store := newChatStoreAdapter(chatClient, cfg.GRPCClient.Timeout)

	upgrader := websocket.Upgrader{
		ReadBufferSize:  wsReadBufferSize,
		WriteBufferSize: wsWriteBufferSize,

		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	manager := hub.NewManager(cfg.Manager, cfg.RateLimit.WSMessage, store, options.shutdownTimeout)
	manager.SetRoomLeaseStore(options.roomLeaseStore)
	if options.sequenceFloor != nil {
		manager.SetSequenceFloorStore(options.sequenceFloor)
	}

	r := &Router{
		mux:            http.NewServeMux(),
		upgrader:       upgrader,
		advertisedAddr: cfg.AdvertisedAddr,
		hashRing:       hashRing,
		userClient:     userClient,
		chatHealth:     options.chatHealth,
		userHealth:     options.userHealth,
		redisClient:    options.redisClient,
		manager:        manager,
		store:          store,
	}

	r.registerRoutes()

	return r
}

func (r *Router) RunManager(ctx context.Context) {
	r.manager.Run(ctx)
}

func (r *Router) WatchOwnership(ctx context.Context, events <-chan struct{}) {
	r.manager.WatchOwnership(ctx, r.hashRing, r.advertisedAddr, events)
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) registerRoutes() {
	r.mux.HandleFunc("GET /health", func(w http.ResponseWriter, req *http.Request) {
		httpio.WriteJSON(req.Context(), w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	r.mux.HandleFunc("GET /ready", r.handleReady)

	r.mux.HandleFunc("GET /ws", r.serveWebSocket)

	r.mux.HandleFunc("POST /internal/rooms/{id}/broadcast", r.handleBroadcast)
	r.mux.HandleFunc("DELETE /internal/rooms/{id}", r.handleForceCloseRoom)
}

func (r *Router) handleReady(w http.ResponseWriter, req *http.Request) {
	failures := r.readinessFailures(req.Context())
	if len(failures) > 0 {
		httpio.WriteProblem(req.Context(), w, http.StatusServiceUnavailable, "not ready: "+strings.Join(failures, "; "))
		return
	}
	httpio.WriteJSON(req.Context(), w, http.StatusOK, map[string]string{"status": "ready"})
}

func (r *Router) readinessFailures(ctx context.Context) []string {
	failures := make([]string, 0, 5)

	if r.manager.Stopped() {
		failures = append(failures, "manager stopped")
	}

	if r.redisClient == nil {
		failures = append(failures, "redis client not configured")
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
		if err := r.redisClient.Ping(checkCtx).Err(); err != nil {
			failures = append(failures, fmt.Sprintf("redis ping failed: %v", err))
		}
		cancel()
	}

	if err := checkGRPCHealth(ctx, r.chatHealth, "chat.v1.ChatService"); err != nil {
		failures = append(failures, fmt.Sprintf("chat-service health failed: %v", err))
	}
	if err := checkGRPCHealth(ctx, r.userHealth, "user.v1.UserService"); err != nil {
		failures = append(failures, fmt.Sprintf("user-service health failed: %v", err))
	}

	if r.hashRing.Len() == 0 {
		failures = append(failures, "hash ring empty")
	} else if !r.hashRing.Contains(r.advertisedAddr) {
		failures = append(failures, "self not observed in hash ring")
	}

	if usage := r.manager.PersistQueueUtilization(); usage >= persistQueueReadyThreshold {
		failures = append(failures, fmt.Sprintf("persist queue utilization %.2f >= %.2f", usage, persistQueueReadyThreshold))
	}

	return failures
}

func checkGRPCHealth(ctx context.Context, client grpc_health_v1.HealthClient, service string) error {
	if client == nil {
		return fmt.Errorf("health client not configured")
	}

	checkCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()

	resp, err := client.Check(checkCtx, &grpc_health_v1.HealthCheckRequest{Service: service})
	if err != nil {
		return err
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}
