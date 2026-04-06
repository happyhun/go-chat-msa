package websocket

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/event"
	"go-chat-msa/internal/websocket/hub"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouter_HandleBroadcast(t *testing.T) {
	t.Parallel()

	manager := hub.NewManager(config.ManagerConfig{
		WriteWait:   10 * time.Second,
		PongWait:    60 * time.Second,
		PingPeriod:  54 * time.Second,
		IdleTimeout: 5 * time.Minute,
		MaxLength:   10000,
	}, config.RateLimitConfig{}, nil)
	go manager.Run(t.Context())

	r := &Router{
		manager: manager,
	}

	handler := http.HandlerFunc(r.handleBroadcast)

	tests := []struct {
		name         string
		url          string
		body         event.BroadcastSystemMessageRequest
		expectedCode int
	}{
		{
			name: "Success: 시스템 메시지 브로드캐스트 성공",
			url:  "/internal/rooms/test-room/broadcast",
			body: event.BroadcastSystemMessageRequest{
				Username: "user-1",
				Event:    "join",
			},
			expectedCode: http.StatusNoContent,
		},
		{
			name: "Failure: 유저네임 누락 (BadRequest)",
			url:  "/internal/rooms/test-room/broadcast",
			body: event.BroadcastSystemMessageRequest{
				Event: "join",
			},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Failure: 빈 요청 바디 (BadRequest)",
			url:          "/internal/rooms/test-room/broadcast",
			body:         event.BroadcastSystemMessageRequest{},
			expectedCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(tt.body)
			require.NoError(t, err)

			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", tt.url, bytes.NewBuffer(b))
			req.Header.Set("Content-Type", "application/json")

			req.SetPathValue("id", "test-room")

			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}
