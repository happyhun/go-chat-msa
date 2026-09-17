package websocket

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-chat-msa/internal/shared/event"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testInternalSecret = "internal-secret"

func newInternalTestRouter(t *testing.T, bus *fakeBus) *Router {
	t.Helper()
	return NewRouter(nil, createTestConfig(), bus,
		WithPodName("test-pod"),
		WithInternalSecret(testInternalSecret))
}

func TestRouter_HandleSystemMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		roomID       string
		body         event.BroadcastSystemMessageRequest
		secret       string
		publishErr   error
		expectedCode int
	}{
		{
			name:         "Success: 입장 시스템 메시지 발행",
			roomID:       testRoomID,
			body:         event.BroadcastSystemMessageRequest{Username: "user-1", Event: event.SystemEventJoin},
			secret:       testInternalSecret,
			expectedCode: http.StatusNoContent,
		},
		{
			name:         "Failure: 시크릿 불일치",
			roomID:       testRoomID,
			body:         event.BroadcastSystemMessageRequest{Username: "user-1", Event: event.SystemEventJoin},
			secret:       "wrong",
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "Failure: 룸 ID가 UUID 형식이 아님",
			roomID:       "test-room",
			body:         event.BroadcastSystemMessageRequest{Username: "user-1", Event: event.SystemEventJoin},
			secret:       testInternalSecret,
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Failure: 유저네임 누락",
			roomID:       testRoomID,
			body:         event.BroadcastSystemMessageRequest{Event: event.SystemEventJoin},
			secret:       testInternalSecret,
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "Failure: 발행 실패는 일시 오류(503)",
			roomID:       testRoomID,
			body:         event.BroadcastSystemMessageRequest{Username: "user-1", Event: event.SystemEventJoin},
			secret:       testInternalSecret,
			publishErr:   errors.New("no responders"),
			expectedCode: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router := newInternalTestRouter(t, &fakeBus{connected: true, publishErr: tt.publishErr})

			b, err := json.Marshal(tt.body)
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/internal/rooms/"+tt.roomID+"/system-messages", bytes.NewBuffer(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Internal-Secret", tt.secret)

			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedCode, w.Code)
		})
	}
}

func TestRouter_HandleCloseRoomSessions(t *testing.T) {
	t.Parallel()

	t.Run("Success: room_closed 이벤트 발행", func(t *testing.T) {
		t.Parallel()

		bus := &fakeBus{connected: true}
		router := newInternalTestRouter(t, bus)

		req := httptest.NewRequest(http.MethodDelete, "/internal/rooms/"+testRoomID+"/sessions", nil)
		req.Header.Set("X-Internal-Secret", testInternalSecret)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNoContent, w.Code)
		require.Len(t, bus.events, 1)
		assert.Equal(t, testRoomID, bus.events[0].RoomID)
	})

	t.Run("Failure: 발행 실패는 일시 오류(503)", func(t *testing.T) {
		t.Parallel()

		router := newInternalTestRouter(t, &fakeBus{connected: true, publishErr: errors.New("no responders")})

		req := httptest.NewRequest(http.MethodDelete, "/internal/rooms/"+testRoomID+"/sessions", nil)
		req.Header.Set("X-Internal-Secret", testInternalSecret)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})

	t.Run("Failure: 시크릿 누락", func(t *testing.T) {
		t.Parallel()

		router := newInternalTestRouter(t, &fakeBus{connected: true})

		req := httptest.NewRequest(http.MethodDelete, "/internal/rooms/"+testRoomID+"/sessions", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}
