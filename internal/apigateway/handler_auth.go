package apigateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/event"
	"go-chat-msa/internal/shared/httpio"
	"go-chat-msa/internal/shared/middleware"
)

type SignupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type SignupResponse struct {
	UserID string `json:"user_id"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

type RefreshResponse struct {
	AccessToken string `json:"access_token"`
}

type DeleteUserRequest struct {
	Password string `json:"password"`
}

const secondsPerDay = int(24 * time.Hour / time.Second)

func (r *Router) handleSignup(w http.ResponseWriter, req *http.Request) {
	var body SignupRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		slog.WarnContext(req.Context(), "ReadJSON failed in handleSignup", "error", err)
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

	httpio.WriteJSON(req.Context(), w, http.StatusCreated, SignupResponse{UserID: resp.UserId})
}

func (r *Router) handleLogin(w http.ResponseWriter, req *http.Request) {
	var body LoginRequest
	if err := httpio.ReadJSON(req.Context(), w, req, &body); err != nil {
		slog.WarnContext(req.Context(), "ReadJSON failed in handleLogin", "error", err)
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

	httpio.WriteJSON(req.Context(), w, http.StatusOK, LoginResponse{
		UserID:      resp.UserId,
		AccessToken: resp.AccessToken,
	})
}

func (r *Router) handleRefresh(w http.ResponseWriter, req *http.Request) {
	cookie, err := req.Cookie("refresh_token")
	if err != nil {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "missing refresh token")
		return
	}

	resp, err := r.userClient.RefreshToken(req.Context(), &userpb.RefreshTokenRequest{
		RefreshToken: cookie.Value,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	r.setRefreshTokenCookie(w, resp.RefreshToken)

	httpio.WriteJSON(req.Context(), w, http.StatusOK, RefreshResponse{
		AccessToken: resp.AccessToken,
	})
}

func (r *Router) handleLogout(w http.ResponseWriter, req *http.Request) {
	cookie, err := req.Cookie("refresh_token")
	if err != nil {
		httpio.WriteProblem(req.Context(), w, http.StatusUnauthorized, "missing refresh token")
		return
	}

	_, err = r.userClient.RevokeToken(req.Context(), &userpb.RevokeTokenRequest{
		RefreshToken: cookie.Value,
	})
	if err != nil {
		writeProblemFromGRPC(w, req, err)
		return
	}

	r.clearRefreshTokenCookie(w)

	w.WriteHeader(http.StatusNoContent)
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

func (r *Router) setRefreshTokenCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    token,
		HttpOnly: true,
		Secure:   r.config.Env == "prod",
		SameSite: http.SameSiteStrictMode,
		Path:     "/auth",
		MaxAge:   r.config.UserService.Token.RefreshTokenExpirationDays * secondsPerDay,
	})
}

func (r *Router) clearRefreshTokenCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    "",
		HttpOnly: true,
		Secure:   r.config.Env == "prod",
		SameSite: http.SameSiteStrictMode,
		Path:     "/auth",
		MaxAge:   -1,
	})
}
