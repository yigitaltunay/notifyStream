package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

func SlogAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		rid := strings.TrimSpace(middleware.GetReqID(r.Context()))
		if rid == "" {
			rid = strings.TrimSpace(r.Header.Get("X-Request-ID"))
		}
		l := slog.Default()
		if rid != "" {
			l = l.With("correlation_id", rid)
		}
		next.ServeHTTP(ww, r)
		l.Info("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
