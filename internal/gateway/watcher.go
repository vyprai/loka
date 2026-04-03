package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// RouteUpdate represents a route change from the control plane.
type RouteUpdate struct {
	Action string        `json:"action"` // "full_sync", "add", "remove", "update"
	Routes []*RouteEntry `json:"routes"`
}

// RouteWatcher connects to the control plane and watches for route changes.
// It uses HTTP long-polling (upgradeable to gRPC streaming later) for simplicity.
// On disconnect, it retries with exponential backoff while the gateway keeps
// serving with its last known routes.
type RouteWatcher struct {
	cpAddr  string // Control plane address (e.g., "http://localhost:6840")
	gateway *Gateway
	logger  *slog.Logger
	client  *http.Client
}

// NewRouteWatcher creates a watcher that syncs routes from the control plane.
func NewRouteWatcher(cpAddr string, gw *Gateway, logger *slog.Logger) *RouteWatcher {
	return &RouteWatcher{
		cpAddr:  cpAddr,
		gateway: gw,
		logger:  logger,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Start begins watching for route changes. Blocks until ctx is cancelled.
// On disconnect, retries with exponential backoff (1s → 2s → 4s → ... → 30s max).
// The gateway keeps serving with last known routes during reconnect.
func (w *RouteWatcher) Start(ctx context.Context) {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := w.syncRoutes(ctx); err != nil {
			w.logger.Warn("route sync failed, retrying",
				"error", err,
				"backoff", backoff,
				"routes_cached", w.gateway.RouteCount(),
			)

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			// Exponential backoff.
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Success — reset backoff.
		backoff = 1 * time.Second

		// Poll interval.
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// syncRoutes fetches the full route table from the CP.
func (w *RouteWatcher) syncRoutes(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/internal/gateway/routes", w.cpAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch routes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch routes: HTTP %d", resp.StatusCode)
	}

	var routes []*RouteEntry
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return fmt.Errorf("decode routes: %w", err)
	}

	w.gateway.SetRoutes(routes)
	return nil
}

// StartMetricsReporter periodically pushes active connection metrics to the CP.
func (w *RouteWatcher) StartMetricsReporter(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reportMetrics(ctx)
		}
	}
}

func (w *RouteWatcher) reportMetrics(ctx context.Context) {
	metrics := w.gateway.AllActiveConnections()
	if len(metrics) == 0 {
		return
	}

	data, _ := json.Marshal(metrics)
	url := fmt.Sprintf("%s/api/v1/gateway/metrics", w.cpAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return // Best-effort, don't log every failure.
	}
	resp.Body.Close()
}
