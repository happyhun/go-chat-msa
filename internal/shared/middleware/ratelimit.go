package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/ratelimit"
)

func RateLimitMiddleware(limiter *ratelimit.Limiter, keyFunc func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFunc(r)
			if key == "" {
				slog.WarnContext(r.Context(), "rate limit bypassed: key function returned empty",
					"method", r.Method, "path", r.URL.Path)
				next.ServeHTTP(w, r)
				return
			}

			if !limiter.Allow(key) {
				w.Header().Set("Retry-After", "1")
				httpio.WriteProblem(r.Context(), w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func IPKeyFunc() func(*http.Request) string {
	return func(r *http.Request) string {
		xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
		if xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[0])
		}

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return host
	}
}

func ContextKeyFunc(contextKey any) func(*http.Request) string {
	return func(r *http.Request) string {
		v := r.Context().Value(contextKey)
		if v == nil {
			return ""
		}
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
}
