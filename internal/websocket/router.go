package websocket

import (
	"context"
	"net/http"
	"sync"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/websocket/hub"

	"github.com/gorilla/websocket"
)

const (
	wsReadBufferSize  = 4096
	wsWriteBufferSize = 4096
)

type Router struct {
	mux      *http.ServeMux
	upgrader websocket.Upgrader

	userClient userpb.UserServiceClient
	manager    *hub.Manager
	store      *chatStoreAdapter
}

func NewRouter(chatClient chatpb.ChatServiceClient, userClient userpb.UserServiceClient, cfg WebSocketConfig) *Router {
	store := newChatStoreAdapter(chatClient, cfg.GRPCClient.Timeout)

	upgrader := websocket.Upgrader{
		ReadBufferSize:  wsReadBufferSize,
		WriteBufferSize: wsWriteBufferSize,

		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	r := &Router{
		mux:        http.NewServeMux(),
		upgrader:   upgrader,
		userClient: userClient,
		manager:    hub.NewManager(cfg.Manager, cfg.RateLimit.WSMessage, store),
		store:      store,
	}

	r.registerRoutes()

	return r
}

func (r *Router) RunManager(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() {
		r.store.runRetryWorker(ctx)
	})
	r.manager.Run(ctx)
	wg.Wait()
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) registerRoutes() {
	r.mux.HandleFunc("GET /health", func(w http.ResponseWriter, req *http.Request) {
		httpio.WriteJSON(req.Context(), w, http.StatusOK, map[string]string{"status": "healthy"})
	})

	r.mux.HandleFunc("GET /ws", r.serveWebSocket)

	r.mux.HandleFunc("POST /internal/rooms/{id}/broadcast", r.handleBroadcast)
	r.mux.HandleFunc("DELETE /internal/rooms/{id}", r.handleForceCloseRoom)
}
