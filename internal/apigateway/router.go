package apigateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/ratelimit"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const readinessTimeout = 2 * time.Second

type RouterOption func(*routerOptions)

type routerOptions struct {
	userHealth grpc_health_v1.HealthClient
	chatHealth grpc_health_v1.HealthClient
}

func WithHealthClients(userHealth, chatHealth grpc_health_v1.HealthClient) RouterOption {
	return func(o *routerOptions) {
		o.userHealth = userHealth
		o.chatHealth = chatHealth
	}
}

type Router struct {
	config    *Config
	jwtSecret string

	mux *http.ServeMux

	userClient  userpb.UserServiceClient
	chatClient  chatpb.ChatServiceClient
	userHealth  grpc_health_v1.HealthClient
	chatHealth  grpc_health_v1.HealthClient
	httpClient  *http.Client
	redisClient *redis.Client

	publicLimiter        *ratelimit.RedisLimiter
	authenticatedLimiter *ratelimit.RedisLimiter

	wg sync.WaitGroup
}

func NewRouter(cfg *Config, userClient userpb.UserServiceClient, chatClient chatpb.ChatServiceClient, redisClient *redis.Client, opts ...RouterOption) *Router {
	options := routerOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = cfg.APIGateway.HTTPClient.MaxIdleConns
	tr.MaxIdleConnsPerHost = cfg.APIGateway.HTTPClient.MaxIdleConnsPerHost

	r := &Router{
		config:     cfg,
		jwtSecret:  cfg.JWT.Secret,
		mux:        http.NewServeMux(),
		userClient: userClient,
		chatClient: chatClient,
		userHealth: options.userHealth,
		chatHealth: options.chatHealth,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   cfg.APIGateway.HTTPClient.Timeout,
		},
		redisClient: redisClient,
		publicLimiter: ratelimit.NewRedis(
			redisClient,
			int(math.Ceil(cfg.APIGateway.RateLimit.Public.RPS)),
			cfg.APIGateway.RateLimit.Public.Burst,
		),
		authenticatedLimiter: ratelimit.NewRedis(
			redisClient,
			int(math.Ceil(cfg.APIGateway.RateLimit.Authenticated.RPS)),
			cfg.APIGateway.RateLimit.Authenticated.Burst,
		),
	}

	r.registerRoutes()

	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) Stop() {
}

func (r *Router) Wait() {
	r.wg.Wait()
}

func (r *Router) registerRoutes() {
	publicMws := []func(http.Handler) http.Handler{
		middleware.RateLimitMiddleware(r.publicLimiter, middleware.IPKeyFunc()),
	}

	r.mux.HandleFunc("GET /health", func(w http.ResponseWriter, req *http.Request) {
		httpio.WriteJSON(req.Context(), w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	r.mux.HandleFunc("GET /ready", r.handleReady)
	r.mux.Handle("POST /users", middleware.ChainMiddleware(r.handleCreateUser, publicMws...))
	r.mux.Handle("POST /auth/token", middleware.ChainMiddleware(r.handleVerifyUser, publicMws...))
	r.mux.Handle("POST /auth/token/refresh", middleware.ChainMiddleware(r.handleRefreshToken, publicMws...))
	r.mux.Handle("DELETE /auth/token", middleware.ChainMiddleware(r.handleRevokeToken, publicMws...))

	authMws := []func(http.Handler) http.Handler{
		middleware.BearerAuthMiddleware(r.jwtSecret),
		middleware.RateLimitMiddleware(r.authenticatedLimiter, middleware.ContextKeyFunc(middleware.UserIDKey)),
	}

	r.mux.Handle("DELETE /me", middleware.ChainMiddleware(r.handleDeleteUser, authMws...))
	r.mux.Handle("GET /users", middleware.ChainMiddleware(r.handleBatchGetUsers, authMws...))
	r.mux.Handle("GET /me/rooms", middleware.ChainMiddleware(r.handleListJoinedRooms, authMws...))
	r.mux.Handle("GET /rooms", middleware.ChainMiddleware(r.handleSearchRooms, authMws...))
	r.mux.Handle("POST /rooms", middleware.ChainMiddleware(r.handleCreateRoom, authMws...))
	r.mux.Handle("PATCH /rooms/{id}", middleware.ChainMiddleware(r.handleUpdateRoom, authMws...))
	r.mux.Handle("DELETE /rooms/{id}", middleware.ChainMiddleware(r.handleDeleteRoom, authMws...))
	r.mux.Handle("PUT /rooms/{id}/members/me", middleware.ChainMiddleware(r.handleJoinRoom, authMws...))
	r.mux.Handle("DELETE /rooms/{id}/members/me", middleware.ChainMiddleware(r.handleLeaveRoom, authMws...))
	r.mux.Handle("GET /rooms/{id}/members", middleware.ChainMiddleware(r.handleListRoomMembers, authMws...))
	r.mux.Handle("GET /rooms/{id}/messages", middleware.ChainMiddleware(r.handleListMessages, authMws...))
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
	failures := make([]string, 0, 3)

	if r.redisClient == nil {
		failures = append(failures, "redis client not configured")
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
		if err := r.redisClient.Ping(checkCtx).Err(); err != nil {
			failures = append(failures, fmt.Sprintf("redis ping failed: %v", err))
		}
		cancel()
	}

	if err := checkGRPCHealth(ctx, r.userHealth, "user.v1.UserService"); err != nil {
		failures = append(failures, fmt.Sprintf("user-service health failed: %v", err))
	}
	if err := checkGRPCHealth(ctx, r.chatHealth, "chat.v1.ChatService"); err != nil {
		failures = append(failures, fmt.Sprintf("chat-service health failed: %v", err))
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
