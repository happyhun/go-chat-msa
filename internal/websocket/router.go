package websocket

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/ratelimit"
	"go-chat-msa/internal/shared/wsticket"
	"go-chat-msa/internal/websocket/hub"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const (
	wsReadBufferSize  = 4096
	wsWriteBufferSize = 4096
	readinessTimeout  = 2 * time.Second
)

type RouterOption func(*routerOptions)

type routerOptions struct {
	shutdownTimeout time.Duration
	userHealth      grpc_health_v1.HealthClient
	internalSecret  string
	podName         string
	redisClient     *redis.Client
}

func WithShutdownTimeout(timeout time.Duration) RouterOption {
	return func(o *routerOptions) {
		o.shutdownTimeout = timeout
	}
}

func WithHealthClients(userHealth grpc_health_v1.HealthClient) RouterOption {
	return func(o *routerOptions) {
		o.userHealth = userHealth
	}
}

func WithInternalSecret(secret string) RouterOption {
	return func(o *routerOptions) {
		o.internalSecret = secret
	}
}

func WithRedisClient(client *redis.Client) RouterOption {
	return func(o *routerOptions) {
		o.redisClient = client
	}
}

func WithPodName(name string) RouterOption {
	return func(o *routerOptions) {
		o.podName = name
	}
}

type Router struct {
	mux      *http.ServeMux
	upgrader websocket.Upgrader

	userClient userpb.UserServiceClient
	userHealth grpc_health_v1.HealthClient
	manager    *hub.Manager

	redisClient    *redis.Client
	tickets        *wsticket.Store
	connectLimiter *ratelimit.RedisLimiter
	internalSecret string
}

func NewRouter(
	userClient userpb.UserServiceClient,
	cfg WebSocketConfig,
	bus hub.MessageBus,
	opts ...RouterOption,
) *Router {
	options := routerOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  wsReadBufferSize,
		WriteBufferSize: wsWriteBufferSize,

		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == "" || slices.Contains(cfg.AllowedOrigins, origin)
		},
	}

	manager := hub.NewManager(cfg.Manager, cfg.RateLimit.WSMessage, bus,
		options.podName, cfg.NATS.MaxDeliveryLag, options.shutdownTimeout)

	r := &Router{
		mux:         http.NewServeMux(),
		upgrader:    upgrader,
		userClient:  userClient,
		userHealth:  options.userHealth,
		manager:     manager,
		redisClient: options.redisClient,
		tickets:     wsticket.NewStore(options.redisClient),
		connectLimiter: ratelimit.NewRedis(
			options.redisClient,
			int(math.Ceil(cfg.RateLimit.WSConnect.RPS)),
			cfg.RateLimit.WSConnect.Burst,
		),
		internalSecret: options.internalSecret,
	}

	r.registerRoutes()

	return r
}

func (r *Router) RunManager(ctx context.Context) {
	r.manager.Run(ctx)
}

func (r *Router) Observer() hub.BusObserver {
	return r.manager
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) registerRoutes() {
	r.mux.HandleFunc("GET /health", func(w http.ResponseWriter, req *http.Request) {
		httpio.WriteJSON(req.Context(), w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	r.mux.HandleFunc("GET /ready", r.handleReady)

	wsMws := []func(http.Handler) http.Handler{
		middleware.RateLimitMiddleware(r.connectLimiter, middleware.IPKeyFunc()),
		middleware.TicketAuthMiddleware(r.tickets),
	}
	r.mux.Handle("GET /ws", middleware.ChainMiddleware(r.serveWebSocket, wsMws...))

	internalMws := []func(http.Handler) http.Handler{
		middleware.InternalAuthMiddleware(r.internalSecret),
	}
	r.mux.Handle("POST /internal/rooms/{id}/system-messages",
		middleware.ChainMiddleware(r.handleSystemMessage, internalMws...))
	r.mux.Handle("DELETE /internal/rooms/{id}/sessions",
		middleware.ChainMiddleware(r.handleCloseRoomSessions, internalMws...))
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

	if r.redisClient == nil {
		failures = append(failures, "redis client not configured")
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
		if err := r.redisClient.Ping(checkCtx).Err(); err != nil {
			failures = append(failures, fmt.Sprintf("redis ping failed: %v", err))
		}
		cancel()
	}

	if r.manager.Stopped() {
		failures = append(failures, "manager stopped")
	}

	if !r.manager.BusConnected() {
		failures = append(failures, "nats disconnected")
	}

	if err := checkGRPCHealth(ctx, r.userHealth, "user.v1.UserService"); err != nil {
		failures = append(failures, fmt.Sprintf("user-service health failed: %v", err))
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
