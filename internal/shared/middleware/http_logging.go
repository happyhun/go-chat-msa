package middleware

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"
)

var sensitiveParams = map[string]struct{}{
	"token":         {},
	"password":      {},
	"secret":        {},
	"key":           {},
	"authorization": {},
	"access_token":  {},
	"refresh_token": {},
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
	hijacked     bool
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytesWritten += int64(n)
	return n, err
}

func (w *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("logging: underlying ResponseWriter does not implement http.Hijacker")
	}
	conn, buf, err := h.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, buf, err
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(lw, r)

		if lw.hijacked {
			logHijacked(r)
			return
		}

		latencyMs := time.Since(start).Milliseconds()
		logLevel := slog.LevelInfo
		if lw.statusCode >= 500 {
			logLevel = slog.LevelError
		}

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", lw.statusCode),
			slog.Int64("latency_ms", latencyMs),
			slog.Int64("bytes_written", lw.bytesWritten),
			slog.Int64("content_length", r.ContentLength),
			slog.String("remote_addr", r.RemoteAddr),
			slog.String("user_agent", r.UserAgent()),
		}
		attrs = appendQueryAttr(attrs, r.URL.Query())
		attrs = appendXFFAttr(attrs, r.Header.Get("X-Forwarded-For"))

		slog.LogAttrs(r.Context(), logLevel, "http request", attrs...)
	})
}

func logHijacked(r *http.Request) {
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int64("content_length", r.ContentLength),
		slog.String("remote_addr", r.RemoteAddr),
		slog.String("user_agent", r.UserAgent()),
		slog.Bool("hijacked", true),
	}
	attrs = appendQueryAttr(attrs, r.URL.Query())
	attrs = appendXFFAttr(attrs, r.Header.Get("X-Forwarded-For"))

	slog.LogAttrs(r.Context(), slog.LevelInfo, "http request", attrs...)
}

func appendQueryAttr(attrs []slog.Attr, query url.Values) []slog.Attr {
	if len(query) == 0 {
		return attrs
	}
	return append(attrs, slog.String("query", sanitizeQuery(query)))
}

func appendXFFAttr(attrs []slog.Attr, xff string) []slog.Attr {
	if xff == "" {
		return attrs
	}
	return append(attrs, slog.String("xff", xff))
}

func sanitizeQuery(query url.Values) string {
	sanitized := make(url.Values, len(query))
	for k, vals := range query {
		if _, ok := sensitiveParams[k]; ok {
			masked := make([]string, len(vals))
			for i := range vals {
				masked[i] = "***"
			}
			sanitized[k] = masked
		} else {
			sanitized[k] = vals
		}
	}
	return sanitized.Encode()
}
