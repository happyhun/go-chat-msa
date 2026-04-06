package apigateway

import (
	"log/slog"
	"net/http"
	"time"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
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
