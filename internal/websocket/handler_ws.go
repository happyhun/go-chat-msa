package websocket

import (
	"errors"
	"log/slog"
	"net/http"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/roomlease"
	"go-chat-msa/internal/websocket/hub"

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

	owner := r.hashRing.Locate(roomID)
	if owner != r.advertisedAddr {
		ownerRejectedTotal.Add(req.Context(), 1)
		slog.WarnContext(req.Context(), "self-check rejected request",
			"room_id", roomID, "expected_owner", owner, "my_addr", r.advertisedAddr)
		httpio.WriteProblem(req.Context(), w, http.StatusMisdirectedRequest, "not the owner of this room")
		return
	}

	registration, err := r.manager.PrepareRegister(req.Context(), roomID)
	if err != nil {
		if errors.Is(err, roomlease.ErrBusy) || errors.Is(err, hub.ErrRoomHandoffInProgress) {
			w.Header().Set("Retry-After", "1")
			httpio.WriteProblem(req.Context(), w, http.StatusServiceUnavailable, "room handoff in progress, please retry")
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
		conn.Close()
		return
	}
}
