package apigateway

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/event"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
)

type CreateUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type CreateUserResponse struct {
	UserID string `json:"user_id"`
}

type VerifyUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type VerifyUserResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

type DeleteUserRequest struct {
	Password string `json:"password"`
}

type User struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}

type BatchGetUsersResponse struct {
	Users []User `json:"users"`
}

const maxBatchUserIDs = 100


func (r *Router) handleCreateUser(w http.ResponseWriter, req *http.Request) {
	var body CreateUserRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		slog.WarnContext(req.Context(), "ReadJSON failed in handleCreateUser", "error", err)
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Username == "" || body.Password == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "username and password are required")
		return
	}

	resp, err := r.userClient.CreateUser(req.Context(), &userpb.CreateUserRequest{
		Username: body.Username,
		Password: body.Password,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	httpio.WriteJSON(req.Context(), w, http.StatusCreated, CreateUserResponse{UserID: resp.UserId})
}

func (r *Router) handleVerifyUser(w http.ResponseWriter, req *http.Request) {
	var body VerifyUserRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		slog.WarnContext(req.Context(), "ReadJSON failed in handleVerifyUser", "error", err)
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Username == "" || body.Password == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "username and password are required")
		return
	}

	resp, err := r.userClient.VerifyUser(req.Context(), &userpb.VerifyUserRequest{
		Username: body.Username,
		Password: body.Password,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	r.setRefreshTokenCookie(w, resp.RefreshToken)

	httpio.WriteJSON(req.Context(), w, http.StatusOK, VerifyUserResponse{
		UserID:      resp.UserId,
		AccessToken: resp.AccessToken,
	})
}

func (r *Router) handleDeleteUser(w http.ResponseWriter, req *http.Request) {
	userID, ok := middleware.GetUserID(req.Context())
	if !ok {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body DeleteUserRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		slog.WarnContext(req.Context(), "ReadJSON failed in handleDeleteUser", "error", err)
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Password == "" {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "password is required")
		return
	}

	resp, err := r.userClient.DeleteUser(req.Context(), &userpb.DeleteUserRequest{
		UserId:   userID,
		Password: body.Password,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	r.clearRefreshTokenCookie(w)
	w.WriteHeader(http.StatusNoContent)

	if len(resp.LeftRoomIds) == 0 {
		return
	}

	username := r.getUsername(req.Context())
	bgCtx := context.WithoutCancel(req.Context())
	timeoutCtx, cancel := context.WithTimeout(bgCtx, r.config.APIGateway.HTTPClient.Timeout)

	r.wg.Add(1)
	go func(ctx context.Context, roomIDs []string, username string) {
		defer cancel()
		defer r.wg.Done()
		for _, roomID := range roomIDs {
			r.broadcastSystemMessage(ctx, roomID, username, event.SystemEventLeave)
		}
	}(timeoutCtx, resp.LeftRoomIds, username)
}

func (r *Router) handleBatchGetUsers(w http.ResponseWriter, req *http.Request) {
	ids := req.URL.Query()["ids"]
	if len(ids) == 0 {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "ids parameter is required")
		return
	}
	if len(ids) > maxBatchUserIDs {
		httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "too many ids")
		return
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			httpio.WriteProblem(req.Context(), w, http.StatusBadRequest, "invalid user id")
			return
		}
	}

	resp, err := r.userClient.BatchGetUsers(req.Context(), &userpb.BatchGetUsersRequest{
		UserIds: ids,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	users := make([]User, len(resp.Users))
	for i, u := range resp.Users {
		users[i] = User{UserID: u.Id, Username: u.Username}
	}
	httpio.WriteJSON(req.Context(), w, http.StatusOK, BatchGetUsersResponse{Users: users})
}
