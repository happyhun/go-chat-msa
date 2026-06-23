package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-chat-msa/internal/shared/httpio"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveryMiddlewareWritesProblem(t *testing.T) {
	t.Parallel()

	handler := RecoveryMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Equal(t, "application/problem+json", recorder.Header().Get("Content-Type"))

	var problem httpio.ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	assert.Equal(t, http.StatusInternalServerError, problem.Status)
	assert.Equal(t, "Internal Server Error", problem.Title)
	assert.Equal(t, "internal server error", problem.Detail)
}
