package wsgateway

import (
	"log/slog"
	"net/http"

	"go-chat-msa/internal/shared/httpio"
)

const maxBroadcastPayload = 65536

func (r *Router) handleBroadcast(w http.ResponseWriter, req *http.Request) {
	r.proxyInternalRequest(w, req)
}

func (r *Router) handleCloseRoom(w http.ResponseWriter, req *http.Request) {
	r.proxyInternalRequest(w, req)
}

func (r *Router) proxyInternalRequest(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	if roomID == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room_id is required")
		return
	}

	if req.Body != nil {
		req.Body = http.MaxBytesReader(w, req.Body, maxBroadcastPayload)
	}

	targetAddr := r.hashRing.Locate(roomID)
	if targetAddr == "" {
		if req.Method == http.MethodDelete {
			slog.WarnContext(req.Context(), "No websocket node found for room during proxy", "room_id", roomID)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		slog.ErrorContext(req.Context(), "Room locator returned empty address", "room_id", roomID)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "no websocket server available")
		return
	}

	proxy, ok := r.getOrCreateProxy(targetAddr)
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusBadGateway, "failed to create proxy")
		return
	}
	proxy.ServeHTTP(w, req)
}
