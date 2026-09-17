package middleware

import (
	"context"
	"errors"
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

type fakeTicketConsumer struct {
	userID string
	ok     bool
	err    error
}

func (f fakeTicketConsumer) Consume(context.Context, string) (string, bool, error) {
	return f.userID, f.ok, f.err
}

func TestTicketAuthMiddleware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		consumer   fakeTicketConsumer
		wantStatus int
		wantUserID string
	}{
		{
			name:       "Success: 유효한 티켓이면 사용자 ID를 context에 담는다",
			query:      "?ticket=valid",
			consumer:   fakeTicketConsumer{userID: "user-1", ok: true},
			wantStatus: http.StatusOK,
			wantUserID: "user-1",
		},
		{
			name:       "Failure: 티켓 쿼리 누락",
			consumer:   fakeTicketConsumer{userID: "user-1", ok: true},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "Failure: 만료되었거나 이미 사용한 티켓",
			query:      "?ticket=used",
			consumer:   fakeTicketConsumer{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "Failure: 티켓 저장소 오류",
			query:      "?ticket=valid",
			consumer:   fakeTicketConsumer{err: errors.New("redis down")},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotUserID string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotUserID, _ = GetUserID(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			w := httptest.NewRecorder()
			TicketAuthMiddleware(tt.consumer)(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ws"+tt.query, nil))

			assert.Equal(t, tt.wantStatus, w.Code)
			assert.Equal(t, tt.wantUserID, gotUserID)
		})
	}
}
