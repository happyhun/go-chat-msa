package telemetry

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/pprof"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	httpMeter           = otel.Meter("go-chat-msa/metrics/http")
	httpRequestsTotal   metric.Int64Counter
	httpRequestDuration metric.Float64Histogram
)

func init() {
	var err error
	httpRequestsTotal, err = httpMeter.Int64Counter("gochat_http_requests",
		metric.WithDescription("HTTP 요청 처리 횟수"),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_http_requests", "error", err)
	}
	httpRequestDuration, err = httpMeter.Float64Histogram("gochat_http_request_duration_seconds",
		metric.WithDescription("HTTP 요청 소요 시간(초)"),
		metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .1, .25, .5, 1, 2.5),
	)
	if err != nil {
		slog.WarnContext(context.Background(), "failed to register metric", "name", "gochat_http_request_duration_seconds", "error", err)
	}
}

var uuidPattern = regexp.MustCompile(
	`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
)

func NormalizePath(p string) string {
	return uuidPattern.ReplaceAllString(p, ":id")
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	hijacked   bool
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("metrics: underlying ResponseWriter does not implement http.Hijacker")
	}
	conn, buf, err := h.Hijack()
	if err == nil {
		rw.hijacked = true
	}
	return conn, buf, err
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func MetricsMiddleware(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		path := NormalizePath(r.URL.Path)
		method := r.Method

		start := time.Now()
		rw := newResponseWriter(w)

		pprof.Do(r.Context(), pprof.Labels(
			"service", service,
			"http.method", method,
			"http.route", path,
		), func(ctx context.Context) {
			next.ServeHTTP(rw, r.WithContext(ctx))
		})

		if rw.hijacked {
			return
		}

		code := strconv.Itoa(rw.statusCode)
		attrs := metric.WithAttributes(
			attribute.String("service", service),
			attribute.String("method", method),
			attribute.String("path", path),
			attribute.String("status_code", code),
		)
		httpRequestsTotal.Add(r.Context(), 1, attrs)
		httpRequestDuration.Record(r.Context(), time.Since(start).Seconds(), attrs)
	})
}
