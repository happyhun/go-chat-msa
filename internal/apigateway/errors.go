package apigateway

import (
	"log/slog"
	"net/http"

	"go-chat-msa/internal/shared/httpio"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func writeProblemFromGRPC(w http.ResponseWriter, r *http.Request, err error) {
	st, ok := status.FromError(err)
	if !ok {
		slog.ErrorContext(r.Context(), "Non-gRPC error received", "error", err, "path", r.URL.Path)
		httpio.WriteProblem(r.Context(), w, http.StatusInternalServerError, "internal server error")
		return
	}

	switch st.Code() {
	case codes.NotFound:
		httpio.WriteProblem(r.Context(), w, http.StatusNotFound, st.Message())
	case codes.AlreadyExists:
		httpio.WriteProblem(r.Context(), w, http.StatusConflict, st.Message())
	case codes.InvalidArgument:
		httpio.WriteProblem(r.Context(), w, http.StatusBadRequest, st.Message())
	case codes.Unauthenticated:
		httpio.WriteProblem(r.Context(), w, http.StatusUnauthorized, st.Message())
	case codes.PermissionDenied:
		httpio.WriteProblem(r.Context(), w, http.StatusForbidden, st.Message())
	case codes.FailedPrecondition:
		httpio.WriteProblem(r.Context(), w, http.StatusConflict, st.Message())
	case codes.ResourceExhausted:
		slog.WarnContext(r.Context(), "System Overloaded", "message", st.Message(), "path", r.URL.Path)
		httpio.WriteProblem(r.Context(), w, http.StatusServiceUnavailable, "system overloaded: "+st.Message())
	case codes.DeadlineExceeded:
		slog.WarnContext(r.Context(), "Processing Timeout", "message", st.Message(), "path", r.URL.Path)
		httpio.WriteProblem(r.Context(), w, http.StatusGatewayTimeout, "processing timeout: "+st.Message())
	default:
		slog.ErrorContext(r.Context(), "Internal Server Error (gRPC)", "code", st.Code(), "message", st.Message(), "path", r.URL.Path)
		httpio.WriteProblem(r.Context(), w, http.StatusInternalServerError, st.Message())
	}
}
