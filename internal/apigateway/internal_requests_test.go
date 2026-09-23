package apigateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/apigateway/mocks"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/shared/middleware"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestInternalRequestsEscapeRoomIDAndRejectRedirects(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"broadcast", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			var redirected atomic.Bool
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirected.Store(true)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.RequestURI
				assert.Equal(t, "internal-secret", r.Header.Get("X-Internal-Secret"))
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			address, err := url.Parse(server.URL)
			require.NoError(t, err)
			host, port, err := net.SplitHostPort(address.Host)
			require.NoError(t, err)
			cfg := &Config{
				Registry:   ServiceRegistry{WebSocket: config.HostConfig{Host: host}},
				Port:       config.PortConfig{WebSocket: port},
				Internal:   config.InternalConfig{Secret: "internal-secret"},
				APIGateway: GatewayConfig{HTTPClient: config.HTTPClientConfig{Timeout: time.Second}},
			}
			userClient := mocks.NewMockUserServiceClient(t)
			router := NewRouter(cfg, userClient, nil, newTestRedisClient(t))
			defer router.httpClient.CloseIdleConnections()
			const roomID = "room/other?query=1#fragment"
			suffix := "system-messages"
			if operation == "broadcast" {
				router.broadcastSystemMessage(t.Context(), roomID, "user", "join")
			} else {
				suffix = "sessions"
				userClient.EXPECT().DeleteRoom(mock.Anything, &userpb.DeleteRoomRequest{RoomId: roomID, RequesterId: "user"}).
					Return(&userpb.DeleteRoomResponse{}, nil)
				req := httptest.NewRequest(http.MethodDelete, "/rooms/room", nil)
				req.SetPathValue("id", roomID)
				req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, "user"))
				recorder := httptest.NewRecorder()
				router.handleDeleteRoom(recorder, req)
				router.wg.Wait()
				assert.Equal(t, http.StatusNoContent, recorder.Code)
			}
			select {
			case uri := <-requests:
				assert.Equal(t, "/internal/rooms/room%2Fother%3Fquery=1%23fragment/"+suffix, uri)
			default:
				t.Fatal("internal request was not received")
			}
			assert.False(t, redirected.Load())
		})
	}
}
