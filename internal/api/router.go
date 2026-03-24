package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func NewRouter(h *Handler, hub *WSHub) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(SlogAccessLog)
	r.Use(middleware.Recoverer)
	r.Use(requestIDToHeader)

	r.Get("/healthz", healthz)
	r.Get("/readyz", h.Readyz)
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/swagger/*", httpSwagger.WrapHandler)

	r.Route("/v1", func(r chi.Router) {
		r.Post("/notifications", h.CreateNotifications)
		r.Get("/notifications", h.ListNotifications)
		r.Get("/notifications/{id}", h.GetNotification)
		r.Post("/notifications/{id}/cancel", h.CancelNotification)
		r.Get("/batches/{batchId}/notifications", h.ListBatchNotifications)
		r.Post("/templates", h.CreateTemplate)
		if hub != nil {
			r.Get("/ws", hub.HandleWS)
		}
	})

	return otelhttp.NewHandler(r, "notifystream-api")
}

// @Summary Liveness
// @Tags health
// @Success 200 {string} string "plain text"
// @Router /healthz [get]
func healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func requestIDToHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rid := middleware.GetReqID(r.Context()); rid != "" {
			w.Header().Set("X-Request-ID", rid)
		}
		next.ServeHTTP(w, r)
	})
}
