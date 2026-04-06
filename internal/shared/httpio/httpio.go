package httpio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

const maxRequestBodySize = 1_048_576

type ProblemDetail struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func ReadJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err != nil {
		var syntaxError *json.SyntaxError
		var unmarshalTypeError *json.UnmarshalTypeError

		switch {
		case errors.As(err, &syntaxError):
			return fmt.Errorf("request body contains badly-formed JSON (at position %d)", syntaxError.Offset)
		case errors.Is(err, io.ErrUnexpectedEOF):
			return errors.New("request body contains badly-formed JSON")
		case errors.As(err, &unmarshalTypeError):
			return fmt.Errorf("request body contains an invalid value for the %q field (at position %d)", unmarshalTypeError.Field, unmarshalTypeError.Offset)
		case errors.Is(err, io.EOF):
			return errors.New("request body must not be empty")
		case errors.As(err, new(*http.MaxBytesError)):
			return errors.New("request body must not be larger than 1MB")
		default:
			return err
		}
	}

	err = dec.Decode(&struct{}{})
	if !errors.Is(err, io.EOF) {
		return errors.New("request body must only contain a single JSON object")
	}

	return nil
}

func WriteJSON(ctx context.Context, w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.ErrorContext(ctx, "failed to write JSON response", "error", err)
	}
}

func WriteProblem(ctx context.Context, w http.ResponseWriter, status int, detail string) {
	problem := ProblemDetail{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(problem); err != nil {
		slog.ErrorContext(ctx, "failed to write problem response", "error", err)
	}
}
