package websocket

import (
	"errors"
	"log/slog"
	"net/http"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
	"go-chat-msa/internal/websocket/hub"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (r *Router) serveWebSocket(w http.ResponseWriter, req *http.Request) {
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "missing user identity")
		return
	}

	roomID := req.URL.Query().Get("room_id")
	if roomID == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "missing room_id query parameter")
		return
	}
	if !isCanonicalUUID(roomID) {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room_id must be a canonical uuid")
		return
	}

	_, err := r.userClient.VerifyRoomMember(req.Context(), &userpb.VerifyRoomMemberRequest{
		RoomId: roomID,
		UserId: userID,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			httpio.WriteProblem(req.Context(), w, http.StatusForbidden, "not a member of the room")
			return
		}
		slog.WarnContext(req.Context(), "VerifyRoomMember RPC failed", "error", err, "room_id", roomID, "user_id", userID)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to verify room membership")
		return
	}

	registration, err := r.manager.PrepareRegister(req.Context(), roomID)
	if err != nil {
		if errors.Is(err, hub.ErrRoomUnavailable) || errors.Is(err, hub.ErrBusUnavailable) || errors.Is(err, hub.ErrManagerStopped) {
			w.Header().Set("Retry-After", "1")
			httpio.WriteProblem(req.Context(), w, http.StatusServiceUnavailable, "room temporarily unavailable, please retry")
			return
		}
		slog.ErrorContext(req.Context(), "Manager.PrepareRegister failed", "error", err, "room_id", roomID, "user_id", userID)
		httpio.WriteProblem(req.Context(), w, http.StatusInternalServerError, "failed to prepare websocket registration")
		return
	}

	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		registration.Cancel()
		slog.ErrorContext(req.Context(), "WebSocket upgrade failed", "error", err, "room_id", roomID, "user_id", userID)
		return
	}

	if err := registration.Commit(req.Context(), conn, userID); err != nil {
		registration.Cancel()
		slog.ErrorContext(req.Context(), "Manager.CommitRegister failed", "error", err, "room_id", roomID, "user_id", userID)
		_ = conn.Close()
		return
	}
}

func isCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}
