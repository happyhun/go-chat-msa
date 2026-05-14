package wsgateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go-chat-msa/internal/wsgateway/loadbalance"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

func TestRouter_Ready(t *testing.T) {
	t.Parallel()

	t.Run("Success: redis and membership ready", func(t *testing.T) {
		t.Parallel()
		mr := miniredis.RunT(t)
		redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })

		r := NewRouter(testConfig(), loadbalance.New([]string{"node-1"}), &fakeRingRefresher{observed: true}, redisClient)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("Failure: ring empty", func(t *testing.T) {
		t.Parallel()
		mr := miniredis.RunT(t)
		redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })

		r := NewRouter(testConfig(), loadbalance.New(nil), &fakeRingRefresher{observed: true}, redisClient)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
}
