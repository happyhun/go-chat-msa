package wsgateway

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"

	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/shared/ratelimit"
	"go-chat-msa/internal/wsgateway/loadbalance"
)

type Router struct {
	config         *Config
	jwtSecret      string
	internalSecret string

	mux                *http.ServeMux
	transport          *http.Transport
	hashRing           *loadbalance.HashRing
	ticketStore        *TicketStore
	publicLimiter      *ratelimit.Limiter
	wsEstablishLimiter *ratelimit.Limiter

	mu      sync.RWMutex
	proxies map[string]*httputil.ReverseProxy
}

func NewRouter(cfg *Config, hashRing *loadbalance.HashRing) *Router {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = cfg.WSGateway.HTTPClient.MaxIdleConns
	tr.MaxIdleConnsPerHost = cfg.WSGateway.HTTPClient.MaxIdleConnsPerHost

	r := &Router{
		config:         cfg,
		jwtSecret:      cfg.JWT.Secret,
		internalSecret: cfg.Internal.Secret,
		mux:            http.NewServeMux(),
		hashRing:       hashRing,
		transport:      tr,
		proxies:        make(map[string]*httputil.ReverseProxy),
		ticketStore: NewTicketStore(),
		publicLimiter: ratelimit.New(
			cfg.WSGateway.RateLimit.Public.RPS,
			cfg.WSGateway.RateLimit.Public.Burst,
			cfg.WSGateway.RateLimit.Public.TTL,
		),
		wsEstablishLimiter: ratelimit.New(
			cfg.WSGateway.RateLimit.WSEstablish.RPS,
			cfg.WSGateway.RateLimit.WSEstablish.Burst,
			cfg.WSGateway.RateLimit.WSEstablish.TTL,
		),
	}

	r.registerRoutes()

	return r
}

func (r *Router) Stop() {
	r.ticketStore.Stop()
	r.publicLimiter.Stop()
	r.wsEstablishLimiter.Stop()
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

	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		slog.ErrorContext(req.Context(), "Reverse proxy failed", "error", err, "target", targetAddr, "path", req.URL.Path)
		httpio.WriteProblem(req.Context(), rw, http.StatusBadGateway, "reverse proxy error")
	}

	r.proxies[targetAddr] = proxy
	return proxy, true
}
