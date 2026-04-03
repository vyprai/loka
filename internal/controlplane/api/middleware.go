package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/vyprai/loka/internal/controlplane/metrics"
	"github.com/vyprai/loka/internal/store"
)

type ctxKeyLogger struct{}

// contextLoggerMiddleware creates a per-request logger with request ID and stores it in context.
func contextLoggerMiddleware(baseLogger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := middleware.GetReqID(r.Context())
			logger := baseLogger
			if reqID != "" {
				logger = baseLogger.With("request_id", reqID)
			}
			ctx := context.WithValue(r.Context(), ctxKeyLogger{}, logger)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// LoggerFromContext extracts the per-request logger. Falls back to slog.Default().
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

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

// authRateLimiter tracks failed auth attempts per IP.
type authRateLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

var globalAuthLimiter = newAuthRateLimiter()

func newAuthRateLimiter() *authRateLimiter {
	rl := &authRateLimiter{failures: make(map[string][]time.Time)}
	// Periodic cleanup every 60s to prevent memory leak under DDoS.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			rl.mu.Lock()
			cutoff := time.Now().Add(-rateLimitBlockDuration)
			for k, v := range rl.failures {
				if len(v) == 0 || v[len(v)-1].Before(cutoff) {
					delete(rl.failures, k)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

const (
	rateLimitWindow  = 60 * time.Second
	rateLimitMax     = 10
	rateLimitBlockDuration = 5 * time.Minute
)

// recordFailure records a failed auth attempt for an IP.
func (rl *authRateLimiter) recordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.failures[ip] = append(rl.failures[ip], time.Now())

	// Prune map if it grows too large (prevents memory leak from many unique IPs).
	if len(rl.failures) > 10000 {
		cutoff := time.Now().Add(-rateLimitBlockDuration)
		for k, v := range rl.failures {
			if len(v) == 0 || v[len(v)-1].Before(cutoff) {
				delete(rl.failures, k)
			}
		}
	}
}

// isBlocked returns true if the IP has exceeded the rate limit.
func (rl *authRateLimiter) isBlocked(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	attempts := rl.failures[ip]
	if len(attempts) == 0 {
		return false
	}

	// Check if the most recent block is still active.
	if len(attempts) >= rateLimitMax {
		last := attempts[len(attempts)-1]
		if time.Since(last) < rateLimitBlockDuration {
			return true
		}
	}

	// Prune old entries outside the window.
	cutoff := time.Now().Add(-rateLimitWindow)
	var recent []time.Time
	for _, t := range attempts {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	rl.failures[ip] = recent

	return len(recent) >= rateLimitMax
}

func clientIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// adminKeyAuth returns middleware that validates an X-Admin-Token header.
// If the admin key is empty, no admin access is possible.
func adminKeyAuth(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("X-Admin-Token")
			if token == "" {
				writeError(w, http.StatusForbidden, "admin access requires X-Admin-Token header")
				return
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(adminKey)) != 1 {
				writeError(w, http.StatusForbidden, "invalid admin token")
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

			// Rate limit failed auth attempts.
			ip := clientIP(r)
			if globalAuthLimiter.isBlocked(ip) {
				writeError(w, http.StatusTooManyRequests, "too many failed auth attempts, try again later")
				return
			}

			auth := r.Header.Get("Authorization")
			if auth == "" {
				globalAuthLimiter.recordFailure(ip)
				writeError(w, http.StatusUnauthorized, "authorization required")
				return
			}

			token := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
				globalAuthLimiter.recordFailure(ip)
				writeError(w, http.StatusForbidden, "invalid API key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
