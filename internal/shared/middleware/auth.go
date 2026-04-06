package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"go-chat-msa/internal/shared/auth"
	"go-chat-msa/internal/shared/httpio"
)

type contextKey string

const (
	UserIDKey   contextKey = "user_id"
	UsernameKey contextKey = "username"
)

func BearerAuthMiddleware(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenString := extractToken(r)
			if tokenString == "" {
				httpio.WriteProblem(r.Context(), w, http.StatusUnauthorized, "missing token")
				return
			}

			claims, err := auth.VerifyJWT(tokenString, jwtSecret)
			if err != nil {
				httpio.WriteProblem(r.Context(), w, http.StatusUnauthorized, "invalid token")
				return
			}

			ctx := context.WithValue(r.Context(), UserIDKey, claims.Subject)
			ctx = context.WithValue(ctx, UsernameKey, claims.Username)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func InternalAuthMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Internal-Secret")
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				httpio.WriteProblem(r.Context(), w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func GetUserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(UserIDKey).(string)
	return id, ok
}

func GetUsername(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(UsernameKey).(string)
	return name, ok
}

func extractToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		parts := strings.Split(authHeader, " ")
		if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
			return parts[1]
		}
	}

	return ""
}
