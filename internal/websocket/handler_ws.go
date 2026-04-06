package websocket

import (
	"log/slog"
	"net/http"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (r *Router) serveWebSocket(w http.ResponseWriter, req *http.Request) {
	userID := req.Header.Get("X-User-ID")
	if userID == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "missing X-User-ID header")
		return
	}

	roomID := req.URL.Query().Get("room_id")
	if roomID == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "missing room_id query parameter")
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

	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		slog.ErrorContext(req.Context(), "WebSocket upgrade failed", "error", err, "room_id", roomID, "user_id", userID)
		return
	}

	if err := r.manager.Register(req.Context(), conn, userID, roomID); err != nil {
		slog.ErrorContext(req.Context(), "Manager.Register failed", "error", err, "room_id", roomID, "user_id", userID)
		conn.Close()
		return
	}
}
