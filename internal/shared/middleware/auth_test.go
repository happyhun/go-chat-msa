package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInternalAuthMiddleware(t *testing.T) {
	t.Parallel()

	const secret = "test-internal-secret"

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := InternalAuthMiddleware(secret)(next)

	tests := []struct {
		name       string
		headerVal  string
		wantStatus int
	}{
		{
			name:       "Success: 유효한 내부 비밀키를 헤더에 포함한 경우",
			headerVal:  secret,
			wantStatus: http.StatusOK,
		},
		{
			name:       "Failure: 잘못된 내부 비밀키를 헤더에 포함한 경우",
			headerVal:  "wrong-secret",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "Failure: 내부 비밀키 헤더가 누락된 경우",
			headerVal:  "",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.headerVal != "" {
				req.Header.Set("X-Internal-Secret", tt.headerVal)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}
