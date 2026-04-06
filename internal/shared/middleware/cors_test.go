package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCORSMiddleware(t *testing.T) {
	t.Parallel()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name           string
		allowedOrigins []string
		origin         string
		method         string
		wantOrigin     string
		wantVary       string
		wantCreds      string
		wantStatus     int
	}{
		{
			name:           "Success: 와일드카드(*)를 통한 모든 오리진 허용",
			allowedOrigins: []string{"*"},
			origin:         "http://example.com",
			method:         http.MethodGet,
			wantOrigin:     "*",
			wantVary:       "Origin",
			wantStatus:     http.StatusOK,
		},
		{
			name:           "Success: 특정 오리진 일치 및 허용",
			allowedOrigins: []string{"http://localhost:3000"},
			origin:         "http://localhost:3000",
			method:         http.MethodGet,
			wantOrigin:     "http://localhost:3000",
			wantVary:       "Origin",
			wantCreds:      "true",
			wantStatus:     http.StatusOK,
		},
		{
			name:           "Failure: 오리진 불일치로 인한 CORS 거부",
			allowedOrigins: []string{"http://localhost:3000"},
			origin:         "http://evil.com",
			method:         http.MethodGet,
			wantOrigin:     "",
			wantVary:       "Origin",
			wantStatus:     http.StatusOK,
		},
		{
			name:           "Success: Preflight (OPTIONS) 요청에 대한 204 응답 확인",
			allowedOrigins: []string{"*"},
			origin:         "http://example.com",
			method:         http.MethodOptions,
			wantOrigin:     "*",
			wantVary:       "Origin",
			wantStatus:     http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := CORSMiddleware(tt.allowedOrigins)(next)
			req := httptest.NewRequest(tt.method, "/", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
			assert.Equal(t, tt.wantOrigin, w.Header().Get("Access-Control-Allow-Origin"))
			if tt.wantVary != "" {
				assert.Equal(t, tt.wantVary, w.Header().Get("Vary"))
			}
			if tt.wantCreds != "" {
				assert.Equal(t, tt.wantCreds, w.Header().Get("Access-Control-Allow-Credentials"))
			}
		})
	}
}
