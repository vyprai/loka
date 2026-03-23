package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rizqme/loka/internal/controlplane/metrics"
	"github.com/rizqme/loka/internal/store"
)

// metricsMiddleware records request count and latency per route.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		duration := time.Since(start).Seconds()

		// Use the chi route pattern for consistent metric labels.
		routePattern := chi.RouteContext(r.Context()).RoutePattern()
		if routePattern == "" {
			routePattern = r.URL.Path
		}

		metrics.APIRequests.WithLabelValues(r.Method, routePattern, fmt.Sprintf("%d", ww.status)).Inc()
		metrics.APILatency.WithLabelValues(r.Method, routePattern).Observe(duration)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// workerTokenAuth returns middleware that validates worker tokens from the Authorization header.
// It looks up the Bearer token in the store and rejects requests with missing, expired, or invalid tokens.
func workerTokenAuth(db store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
				writeError(w, http.StatusUnauthorized, "invalid worker token")
				return
			}

			token := strings.TrimPrefix(auth, "Bearer ")
			wt, err := db.Tokens().GetByToken(r.Context(), token)
			if err != nil || wt == nil || !wt.IsValid() {
				writeError(w, http.StatusUnauthorized, "invalid worker token")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// apiKeyAuth returns middleware that validates API key from Authorization header.
// If apiKey is empty, authentication is disabled.
func apiKeyAuth(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Skip auth for health and metrics endpoints.
			if r.URL.Path == "/api/v1/health" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			auth := r.Header.Get("Authorization")
			if auth == "" {
				writeError(w, http.StatusUnauthorized, "authorization required")
				return
			}

			token := strings.TrimPrefix(auth, "Bearer ")
			if token != apiKey {
				writeError(w, http.StatusForbidden, "invalid API key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
