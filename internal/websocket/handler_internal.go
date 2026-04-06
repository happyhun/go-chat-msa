package websocket

import (
	"fmt"
	"log/slog"
	"net/http"

	"go-chat-msa/internal/shared/event"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/websocket/hub"
)

func (r *Router) handleBroadcast(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	if roomID == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room_id is required")
		return
	}

	var body event.BroadcastSystemMessageRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		slog.WarnContext(req.Context(), "Invalid broadcast request", "error", err)
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Username == "" || body.Event == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "username and event are required")
		return
	}

	var content string
	switch body.Event {
	case event.SystemEventJoin:
		content = fmt.Sprintf("%s님이 들어왔습니다.", body.Username)
	case event.SystemEventLeave:
		content = fmt.Sprintf("%s님이 나갔습니다.", body.Username)
	default:
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid system message type")
		return
	}

	msg, err := hub.NewSystemMessage(roomID, content)
	if err != nil {
		slog.ErrorContext(req.Context(), "Failed to create system message", "error", err)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to create system message")
		return
	}

	if err := r.manager.Broadcast(req.Context(), msg); err != nil {
		slog.ErrorContext(req.Context(), "Manager.Broadcast failed", "error", err, "room_id", roomID)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to broadcast message")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (r *Router) handleForceCloseRoom(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	if roomID == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room_id is required")
		return
	}

	if err := r.manager.ForceCloseRoom(req.Context(), roomID); err != nil {
		slog.ErrorContext(req.Context(), "Manager.ForceCloseRoom failed", "error", err, "room_id", roomID)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to force close room")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
