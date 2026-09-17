package websocket

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"go-chat-msa/internal/shared/event"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/websocket/hub"
)

func (r *Router) handleSystemMessage(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	if !isCanonicalUUID(roomID) {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room id must be a canonical uuid")
		return
	}

	var body event.BroadcastSystemMessageRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		slog.WarnContext(req.Context(), "Invalid system message request", "error", err)
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

	if err := r.manager.PublishSystemMessage(req.Context(), roomID, content); err != nil {
		if isTransientPublishError(err) {
			slog.WarnContext(req.Context(), "system message publish unavailable", "error", err, "room_id", roomID)
			httpio.WriteProblem(req.Context(), w, http.StatusServiceUnavailable, "publish temporarily unavailable, please retry")
			return
		}
		slog.ErrorContext(req.Context(), "Manager.PublishSystemMessage failed", "error", err, "room_id", roomID)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to publish system message")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (r *Router) handleCloseRoomSessions(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	if !isCanonicalUUID(roomID) {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room id must be a canonical uuid")
		return
	}

	if err := r.manager.CloseRoomSessions(req.Context(), roomID); err != nil {
		if isTransientPublishError(err) {
			slog.WarnContext(req.Context(), "room close event publish unavailable", "error", err, "room_id", roomID)
			httpio.WriteProblem(req.Context(), w, http.StatusServiceUnavailable, "publish temporarily unavailable, please retry")
			return
		}
		slog.ErrorContext(req.Context(), "Manager.CloseRoomSessions failed", "error", err, "room_id", roomID)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to close room sessions")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func isTransientPublishError(err error) bool {
	return errors.Is(err, hub.ErrBusUnavailable) ||
		errors.Is(err, hub.ErrManagerStopped)
}
