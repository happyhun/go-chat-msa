package apigateway

import (
	"net/http"
	"slices"
	"sync"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/ratelimit"
)

type Router struct {
	config    *Config
	jwtSecret string

	muxV1   *http.ServeMux
	muxV2   *http.ServeMux
	handler *middleware.VersionRouter

	userClient userpb.UserServiceClient
	chatClient chatpb.ChatServiceClient
	httpClient *http.Client

	publicLimiter        *ratelimit.Limiter
	authenticatedLimiter *ratelimit.Limiter

	wg sync.WaitGroup
}

func NewRouter(cfg *Config, userClient userpb.UserServiceClient, chatClient chatpb.ChatServiceClient) *Router {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = cfg.APIGateway.HTTPClient.MaxIdleConns
	tr.MaxIdleConnsPerHost = cfg.APIGateway.HTTPClient.MaxIdleConnsPerHost

	r := &Router{
		config:     cfg,
		jwtSecret:  cfg.JWT.Secret,
		muxV1:      http.NewServeMux(),
		muxV2:      http.NewServeMux(),
		userClient: userClient,
		chatClient: chatClient,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   cfg.APIGateway.HTTPClient.Timeout,
		},
		publicLimiter: ratelimit.New(
			cfg.APIGateway.RateLimit.Public.RPS,
			cfg.APIGateway.RateLimit.Public.Burst,
			cfg.APIGateway.RateLimit.Public.TTL,
		),
		authenticatedLimiter: ratelimit.New(
			cfg.APIGateway.RateLimit.Authenticated.RPS,
			cfg.APIGateway.RateLimit.Authenticated.Burst,
			cfg.APIGateway.RateLimit.Authenticated.TTL,
		),
	}

	r.registerV1Routes()
	r.registerV2Routes()

	r.handler = middleware.NewVersionRouter(r.muxV1, map[string]http.Handler{
		"v1": r.muxV1,
		"v2": r.muxV2,
	})

	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

func (r *Router) Stop() {
	r.publicLimiter.Stop()
	r.authenticatedLimiter.Stop()
}

func (r *Router) Wait() {
	r.wg.Wait()
}

func (r *Router) registerV1Routes() {
	globalMws := []func(http.Handler) http.Handler{
		middleware.CORSMiddleware(r.config.APIGateway.CORS.AllowedOrigins),
	}

	publicMws := slices.Concat(globalMws, []func(http.Handler) http.Handler{
		middleware.RateLimitMiddleware(r.publicLimiter, middleware.IPKeyFunc()),
	})

	r.muxV1.Handle("GET /health", middleware.ChainMiddleware(
		func(w http.ResponseWriter, req *http.Request) {
			httpio.WriteJSON(req.Context(), w, http.StatusOK, map[string]string{"status": "healthy"})
		}, publicMws...))
	r.muxV1.Handle("POST /users", middleware.ChainMiddleware(r.handleSignup, publicMws...))
	r.muxV1.Handle("POST /auth/token", middleware.ChainMiddleware(r.handleLogin, publicMws...))
	r.muxV1.Handle("POST /auth/refresh", middleware.ChainMiddleware(r.handleRefresh, publicMws...))
	r.muxV1.Handle("DELETE /auth/token", middleware.ChainMiddleware(r.handleLogout, publicMws...))

	authMws := slices.Concat(globalMws, []func(http.Handler) http.Handler{
		middleware.BearerAuthMiddleware(r.jwtSecret),
		middleware.RateLimitMiddleware(r.authenticatedLimiter, middleware.ContextKeyFunc(middleware.UserIDKey)),
	})

	r.muxV1.Handle("GET /me/rooms", middleware.ChainMiddleware(r.handleListJoinedRooms, authMws...))
	r.muxV1.Handle("GET /rooms", middleware.ChainMiddleware(r.handleSearchRooms, authMws...))
	r.muxV1.Handle("POST /rooms", middleware.ChainMiddleware(r.handleCreateRoom, authMws...))
	r.muxV1.Handle("PATCH /rooms/{id}", middleware.ChainMiddleware(r.handleUpdateRoom, authMws...))
	r.muxV1.Handle("DELETE /rooms/{id}", middleware.ChainMiddleware(r.handleDeleteRoom, authMws...))
	r.muxV1.Handle("PUT /rooms/{id}/members/me", middleware.ChainMiddleware(r.handleJoinRoom, authMws...))
	r.muxV1.Handle("DELETE /rooms/{id}/members/me", middleware.ChainMiddleware(r.handleLeaveRoom, authMws...))
	r.muxV1.Handle("GET /rooms/{id}/members", middleware.ChainMiddleware(r.handleListRoomMembers, authMws...))
	r.muxV1.Handle("GET /rooms/{id}/messages", middleware.ChainMiddleware(r.handleListMessages, authMws...))
}

func (r *Router) registerV2Routes() {
	r.muxV2.Handle("/", r.muxV1)
}
