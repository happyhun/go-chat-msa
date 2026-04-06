package wsgateway

import (
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
)

type TicketResponse struct {
	Ticket string `json:"ticket"`
}

func (r *Router) handleCreateWSTicket(w http.ResponseWriter, req *http.Request) {
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "missing user identity")
		return
	}

	ticket := uuid.NewString()
	r.ticketStore.Set(ticket, userID, r.config.WSGateway.TicketTTL)

	httpio.WriteJSON(req.Context(), w, http.StatusOK, TicketResponse{Ticket: ticket})
}

func (r *Router) proxyWebSocket(w http.ResponseWriter, req *http.Request) {
	ticket := req.URL.Query().Get("ticket")
	if ticket == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "missing ticket")
		return
	}

	userID, ok := r.ticketStore.GetAndDelete(ticket)
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}

	roomID := req.URL.Query().Get("room_id")
	if roomID == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room_id is required")
		return
	}

	targetAddr := r.hashRing.Locate(roomID)
	if targetAddr == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to locate websocket server")
		return
	}
	routedTotal.Add(req.Context(), 1, metric.WithAttributes(attribute.String("endpoint", targetAddr)))

	req.Header.Set("X-User-ID", userID)

	proxy, ok := r.getOrCreateProxy(targetAddr)
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusBadGateway, "failed to create proxy")
		return
	}
	proxy.ServeHTTP(w, req)
}
