package wsgateway

import (
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"

	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/membership"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/ratelimit"
	"go-chat-msa/internal/wsgateway/loadbalance"

	"github.com/redis/go-redis/v9"
)

type Router struct {
	config         *Config
	jwtSecret      string
	internalSecret string

	mux                *http.ServeMux
	transport          *http.Transport
	hashRing           *loadbalance.HashRing
	watcher            *membership.Watcher
	ticketStore        *TicketStore
	publicLimiter      *ratelimit.RedisLimiter
	wsEstablishLimiter *ratelimit.RedisLimiter

	mu      sync.RWMutex
	proxies map[string]*httputil.ReverseProxy
}

func NewRouter(cfg *Config, hashRing *loadbalance.HashRing, watcher *membership.Watcher, redisClient *redis.Client) *Router {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = cfg.WSGateway.HTTPClient.MaxIdleConns
	tr.MaxIdleConnsPerHost = cfg.WSGateway.HTTPClient.MaxIdleConnsPerHost

	r := &Router{
		config:         cfg,
		jwtSecret:      cfg.JWT.Secret,
		internalSecret: cfg.Internal.Secret,
		mux:            http.NewServeMux(),
		hashRing:       hashRing,
		watcher:        watcher,
		transport:      tr,
		proxies:        make(map[string]*httputil.ReverseProxy),
		ticketStore:    NewTicketStore(redisClient),
		publicLimiter: ratelimit.NewRedis(
			redisClient,
			int(math.Ceil(cfg.WSGateway.RateLimit.Public.RPS)),
			cfg.WSGateway.RateLimit.Public.Burst,
		),
		wsEstablishLimiter: ratelimit.NewRedis(
			redisClient,
			int(math.Ceil(cfg.WSGateway.RateLimit.WSEstablish.RPS)),
			cfg.WSGateway.RateLimit.WSEstablish.Burst,
		),
	}

	r.registerRoutes()

	return r
}

func (r *Router) Stop() {
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) registerRoutes() {
	globalMws := []func(http.Handler) http.Handler{
		middleware.CORSMiddleware(r.config.WSGateway.CORS.AllowedOrigins),
	}

	publicMws := slices.Concat(globalMws, []func(http.Handler) http.Handler{
		middleware.RateLimitMiddleware(r.publicLimiter, middleware.IPKeyFunc()),
	})

	r.mux.Handle("GET /health", middleware.ChainMiddleware(
		func(w http.ResponseWriter, req *http.Request) {
			httpio.WriteJSON(req.Context(), w, http.StatusOK, map[string]string{"status": "healthy"})
		}, publicMws...))

	ticketMws := slices.Concat(globalMws, []func(http.Handler) http.Handler{
		middleware.BearerAuthMiddleware(r.jwtSecret),
		middleware.RateLimitMiddleware(r.wsEstablishLimiter, middleware.ContextKeyFunc(middleware.UserIDKey)),
	})
	r.mux.Handle("POST /ws/ticket", middleware.ChainMiddleware(r.handleCreateWSTicket, ticketMws...))
	r.mux.Handle("GET /ws", middleware.ChainMiddleware(r.proxyWebSocket, publicMws...))

	internalMws := []func(http.Handler) http.Handler{
		middleware.InternalAuthMiddleware(r.internalSecret),
	}
	r.mux.Handle("POST /internal/rooms/{id}/broadcast", middleware.ChainMiddleware(r.handleBroadcast, internalMws...))
	r.mux.Handle("DELETE /internal/rooms/{id}", middleware.ChainMiddleware(r.handleCloseRoom, internalMws...))
}

func (r *Router) getOrCreateProxy(targetAddr string) (*httputil.ReverseProxy, bool) {
	r.mu.RLock()
	proxy, ok := r.proxies[targetAddr]
	r.mu.RUnlock()

	if ok {
		return proxy, true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if proxy, ok = r.proxies[targetAddr]; ok {
		return proxy, true
	}

	targetURL, err := url.Parse("http://" + targetAddr)
	if err != nil {
		slog.Error("failed to parse proxy target URL", "target", targetAddr, "error", err)
		return nil, false
	}

	proxy = httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = r.transport
	proxy.FlushInterval = -1

	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode != http.StatusMisdirectedRequest {
			return nil
		}
		r.watcher.ForceReconcile()
		slog.WarnContext(resp.Request.Context(), "misdirected request converted to 503",
			"target", targetAddr, "path", resp.Request.URL.Path)
		resp.StatusCode = http.StatusServiceUnavailable
		resp.Status = http.StatusText(http.StatusServiceUnavailable)
		return nil
	}

	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		slog.ErrorContext(req.Context(), "Reverse proxy failed", "error", err, "target", targetAddr, "path", req.URL.Path)
		httpio.WriteProblem(req.Context(), rw, http.StatusBadGateway, "reverse proxy error")
	}

	r.proxies[targetAddr] = proxy
	return proxy, true
}
