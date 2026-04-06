package apigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	chatpb "go-chat-msa/api/proto/chat/v1"
	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/event"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
)

type CreateRoomRequest struct {
	Name     string `json:"name"`
	Capacity int32  `json:"capacity"`
}

type UpdateRoomRequest struct {
	Name     string `json:"name"`
	Capacity int32  `json:"capacity"`
}

type CreateRoomResponse struct {
	RoomID string `json:"room_id"`
}

type JoinedRoomItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ManagerID   string `json:"manager_id"`
	Capacity    int32  `json:"capacity"`
	MemberCount int32  `json:"member_count"`
	JoinedAt    string `json:"joined_at,omitempty"`
}

type ListJoinedRoomsResponse struct {
	Rooms []JoinedRoomItem `json:"rooms"`
}

type RoomMemberItem struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	JoinedAt string `json:"joined_at"`
}

type ListRoomMembersResponse struct {
	Members []RoomMemberItem `json:"members"`
}

type SearchedRoomItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ManagerID   string `json:"manager_id"`
	Capacity    int32  `json:"capacity"`
	MemberCount int32  `json:"member_count"`
}

type SearchRoomsResponse struct {
	Rooms      []SearchedRoomItem `json:"rooms"`
	TotalCount int64              `json:"total_count"`
}

type MessageItem struct {
	ID             string `json:"id"`
	RoomID         string `json:"room_id"`
	SenderID       string `json:"sender_id"`
	Content        string `json:"content"`
	Type           string `json:"type"`
	Timestamp      int64  `json:"timestamp"`
	SequenceNumber int64  `json:"sequence_number"`
}

type ListMessagesResponse struct {
	Messages []MessageItem `json:"messages"`
}

func (r *Router) handleListJoinedRooms(w http.ResponseWriter, req *http.Request) {
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	resp, err := r.userClient.ListJoinedRooms(req.Context(), &userpb.ListJoinedRoomsRequest{
		UserId: userID,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	rooms := make([]JoinedRoomItem, len(resp.Rooms))
	for i, ur := range resp.Rooms {
		rooms[i] = JoinedRoomItem{
			ID:          ur.Room.Id,
			Name:        ur.Room.Name,
			ManagerID:   ur.Room.ManagerId,
			Capacity:    ur.Room.Capacity,
			MemberCount: ur.Room.MemberCount,
			JoinedAt:    ur.JoinedAt.AsTime().Format(time.RFC3339),
		}
	}

	httpio.WriteJSON(req.Context(), w, http.StatusOK, ListJoinedRoomsResponse{Rooms: rooms})
}

func (r *Router) handleSearchRooms(w http.ResponseWriter, req *http.Request) {
	query := req.URL.Query()
	q := query.Get("q")
	limitStr := query.Get("limit")
	offsetStr := query.Get("offset")

	limit := r.config.UserService.Search.DefaultLimit
	if limitStr != "" {
		l, err := strconv.ParseInt(limitStr, 10, 32)
		if err != nil {
			httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid limit parameter")
			return
		}
		if l <= 0 {
			httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "limit must be positive")
			return
		}
		if int32(l) > r.config.UserService.Search.MaxLimit {
			httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "limit exceeds maximum allowed")
			return
		}
		limit = int32(l)
	}

	offset := int32(0)
	if offsetStr != "" {
		o, err := strconv.ParseInt(offsetStr, 10, 32)
		if err != nil || o < 0 {
			httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid offset parameter")
			return
		}
		offset = int32(o)
	}

	resp, err := r.userClient.SearchRooms(req.Context(), &userpb.SearchRoomsRequest{
		Query:  q,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	rooms := make([]SearchedRoomItem, len(resp.Rooms))
	for i, room := range resp.Rooms {
		rooms[i] = SearchedRoomItem{
			ID:          room.Id,
			Name:        room.Name,
			ManagerID:   room.ManagerId,
			Capacity:    room.Capacity,
			MemberCount: room.MemberCount,
		}
	}

	httpio.WriteJSON(req.Context(), w, http.StatusOK, SearchRoomsResponse{
		Rooms:      rooms,
		TotalCount: int64(resp.TotalCount),
	})
}

func (r *Router) handleCreateRoom(w http.ResponseWriter, req *http.Request) {
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body CreateRoomRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Name == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "room name is required")
		return
	}

	resp, err := r.userClient.CreateRoom(req.Context(), &userpb.CreateRoomRequest{
		Name:      body.Name,
		ManagerId: userID,
		Capacity:  body.Capacity,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	httpio.WriteJSON(req.Context(), w, http.StatusCreated, CreateRoomResponse{RoomID: resp.RoomId})
}

func (r *Router) handleUpdateRoom(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body UpdateRoomRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid request body")
		return
	}

	if _, err := r.userClient.UpdateRoom(req.Context(), &userpb.UpdateRoomRequest{
		Id:          roomID,
		Name:        body.Name,
		Capacity:    body.Capacity,
		RequesterId: userID,
	}); err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (r *Router) handleDeleteRoom(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if _, err := r.userClient.DeleteRoom(req.Context(), &userpb.DeleteRoomRequest{
		RoomId:      roomID,
		RequesterId: userID,
	}); err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)

	bgCtx := context.WithoutCancel(req.Context())
	timeoutCtx, cancel := context.WithTimeout(bgCtx, r.config.APIGateway.HTTPClient.Timeout)

	r.wg.Add(1)
	go func(ctx context.Context, roomID string) {
		defer cancel()
		defer r.wg.Done()

		url := fmt.Sprintf("%s/internal/rooms/%s", r.config.WSGatewayAddr(), roomID)
		proxyReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to create request for room cleanup", "error", err, "room_id", roomID)
			return
		}
		proxyReq.Header.Set("X-Internal-Secret", r.config.Internal.Secret)

		resp, err := r.httpClient.Do(proxyReq)
		if err != nil {
			slog.WarnContext(ctx, "Failed to force close room hub via ws-gateway", "error", err, "room_id", roomID)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
			slog.ErrorContext(ctx, "Unexpected status code from ws-gateway cleanup", "status", resp.StatusCode, "room_id", roomID)
		}
	}(timeoutCtx, roomID)
}

func (r *Router) handleJoinRoom(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if _, err := r.userClient.JoinRoom(req.Context(), &userpb.JoinRoomRequest{
		RoomId: roomID,
		UserId: userID,
	}); err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)

	username := r.getUsername(req.Context())
	bgCtx := context.WithoutCancel(req.Context())
	timeoutCtx, cancel := context.WithTimeout(bgCtx, r.config.APIGateway.HTTPClient.Timeout)

	r.wg.Add(1)
	go func() {
		defer cancel()
		defer r.wg.Done()
		r.broadcastSystemMessage(timeoutCtx, roomID, username, event.SystemEventJoin)
	}()
}

func (r *Router) handleLeaveRoom(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if _, err := r.userClient.LeaveRoom(req.Context(), &userpb.LeaveRoomRequest{
		RoomId: roomID,
		UserId: userID,
	}); err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)

	username := r.getUsername(req.Context())
	bgCtx := context.WithoutCancel(req.Context())
	timeoutCtx, cancel := context.WithTimeout(bgCtx, r.config.APIGateway.HTTPClient.Timeout)

	r.wg.Add(1)
	go func() {
		defer cancel()
		defer r.wg.Done()
		r.broadcastSystemMessage(timeoutCtx, roomID, username, event.SystemEventLeave)
	}()
}

func (r *Router) handleListMessages(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")
	query := req.URL.Query()

	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	membership, err := r.userClient.GetMemberJoinedAt(req.Context(), &userpb.GetMemberJoinedAtRequest{
		RoomId: roomID,
		UserId: userID,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}
	joinedAt := membership.JoinedAt

	lastSeqStr := query.Get("last_seq")
	if lastSeqStr != "" {
		lastSeq, err := strconv.ParseInt(lastSeqStr, 10, 64)
		if err != nil {
			httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid last_seq parameter")
			return
		}
		var limit int64
		if s := query.Get("limit"); s != "" {
			limit, err = strconv.ParseInt(s, 10, 32)
			if err != nil || limit <= 0 {
				httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid limit parameter")
				return
			}
		}
		resp, err := r.chatClient.SyncMessages(req.Context(), &chatpb.SyncMessagesRequest{
			RoomId:             roomID,
			LastSequenceNumber: lastSeq,
			Limit:              int32(limit),
			JoinedAt:           joinedAt,
		})
		if err != nil {
			writeProblemFromGRPC(w, req, err)
			return
		}
		httpio.WriteJSON(req.Context(), w, http.StatusOK, ListMessagesResponse{
			Messages: messageItemsFromProto(resp.Messages),
		})
		return
	}

	var limit int64
	if s := query.Get("limit"); s != "" {
		limit, err = strconv.ParseInt(s, 10, 32)
		if err != nil || limit <= 0 {
			httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid limit parameter")
			return
		}
	}
	resp, err := r.chatClient.ListMessages(req.Context(), &chatpb.ListMessagesRequest{
		RoomId:   roomID,
		Limit:    int32(limit),
		JoinedAt: joinedAt,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}
	httpio.WriteJSON(req.Context(), w, http.StatusOK, ListMessagesResponse{
		Messages: messageItemsFromProto(resp.Messages),
	})
}

func messageItemsFromProto(msgs []*chatpb.Message) []MessageItem {
	items := make([]MessageItem, len(msgs))
	for i, m := range msgs {
		var ts int64
		if m.Timestamp != nil {
			ts = m.Timestamp.Seconds
		}
		items[i] = MessageItem{
			ID:             m.Id,
			RoomID:         m.RoomId,
			SenderID:       m.SenderId,
			Content:        m.Content,
			Type:            m.Type,
			Timestamp:      ts,
			SequenceNumber: m.SequenceNumber,
		}
	}
	return items
}

func (r *Router) handleListRoomMembers(w http.ResponseWriter, req *http.Request) {
	roomID := req.PathValue("id")

	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	_, err := r.userClient.VerifyRoomMember(req.Context(), &userpb.VerifyRoomMemberRequest{
		RoomId: roomID,
		UserId: userID,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	resp, err := r.userClient.ListRoomMembers(req.Context(), &userpb.ListRoomMembersRequest{
		RoomId: roomID,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	members := make([]RoomMemberItem, len(resp.Members))
	for i, m := range resp.Members {
		members[i] = RoomMemberItem{
			UserID:   m.UserId,
			Username: m.Username,
			JoinedAt: m.JoinedAt.AsTime().Format(time.RFC3339),
		}
	}

	httpio.WriteJSON(req.Context(), w, http.StatusOK, ListRoomMembersResponse{Members: members})
}

func (r *Router) getUsername(ctx context.Context) string {
	username, ok := middleware.GetUsername(ctx)
	if !ok || username == "" {
		if userID, ok := middleware.GetUserID(ctx); ok {
			return userID
		}
		return "Unknown"
	}
	return username
}

func (r *Router) broadcastSystemMessage(ctx context.Context, roomID, username, eventType string) {
	reqBody := event.BroadcastSystemMessageRequest{
		Username: username,
		Event:    eventType,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to marshal broadcast system message request", "error", err)
		return
	}

	url := fmt.Sprintf("%s/internal/rooms/%s/broadcast", r.config.WSGatewayAddr(), roomID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create broadcast system message request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", r.config.Internal.Secret)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to send broadcast system message request", "error", err, "url", url)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		slog.ErrorContext(ctx, "Unexpected status from broadcast system message request", "status", resp.StatusCode)
	}
}
