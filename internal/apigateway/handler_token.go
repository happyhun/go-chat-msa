package apigateway

import (
	"net/http"
	"time"

	userpb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/httpio"
)

type RefreshTokenResponse struct {
	AccessToken string `json:"access_token"`
}

const secondsPerDay = int(24 * time.Hour / time.Second)

func (r *Router) handleRefreshToken(w http.ResponseWriter, req *http.Request) {
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

	httpio.WriteJSON(req.Context(), w, http.StatusOK, RefreshTokenResponse{
		AccessToken: resp.AccessToken,
	})
}

func (r *Router) handleRevokeToken(w http.ResponseWriter, req *http.Request) {
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
