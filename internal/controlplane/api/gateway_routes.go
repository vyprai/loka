package api

import (
	"net/http"

	"github.com/vyprai/loka/internal/gateway"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// gatewayRoutes returns the full resolved route table for the gateway.
// Called by lokad --role=gw to sync routes from the control plane.
// GET /api/v1/gateway/routes
func (s *Server) gatewayRoutes(w http.ResponseWriter, r *http.Request) {
	var routes []*gateway.RouteEntry

	// Collect routes from running services.
	running := loka.ServiceStatusRunning
	svcs, _, err := s.store.Services().List(r.Context(), store.ServiceFilter{Status: &running, Limit: 10000})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	for _, svc := range svcs {
		if len(svc.Routes) == 0 {
			continue
		}

		// Resolve worker IP.
		workerIP := resolveWorkerIP(s, svc.WorkerID)

		for _, sr := range svc.Routes {
			routes = append(routes, &gateway.RouteEntry{
				Domain:      sr.Domain,
				ServiceID:   svc.ID,
				ServiceName: svc.Name,
				RemotePort:  sr.Port,
				WorkerIP:    workerIP,
				ForwardPort: svc.ForwardPort,
				GuestIP:     svc.GuestIP,
			})
		}
	}

	// Collect routes from exposed sessions (domain proxy session routes).
	if s.domainProxy != nil {
		for _, dr := range s.domainProxy.ListRoutes() {
			if dr.SessionID == "" {
				continue
			}
			sess, err := s.sessionManager.Get(r.Context(), dr.SessionID)
			if err != nil || sess.Status != loka.SessionStatusRunning {
				continue
			}
			routes = append(routes, &gateway.RouteEntry{
				Domain:     dr.Domain,
				SessionID:  dr.SessionID,
				RemotePort: dr.RemotePort,
				WorkerIP:   resolveWorkerIP(s, sess.WorkerID),
				IsSession:  true,
			})
		}
	}

	writeJSON(w, http.StatusOK, routes)
}

// gatewayMetrics receives active connection metrics from the gateway.
// POST /api/v1/gateway/metrics
func (s *Server) gatewayMetrics(w http.ResponseWriter, r *http.Request) {
	var metrics map[string]int64
	if err := decodeJSON(r, &metrics); err != nil {
		writeError(w, http.StatusBadRequest, "invalid metrics payload")
		return
	}

	// Store metrics so autoscaler can use them.
	if s.domainProxy != nil {
		for serviceID, count := range metrics {
			m := s.domainProxy.GetServiceMetrics(serviceID)
			m.ActiveConnections.Store(count)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func resolveWorkerIP(s *Server, workerID string) string {
	if workerID == "" {
		return ""
	}
	wc, ok := s.workerRegistry.Get(workerID)
	if !ok {
		return ""
	}
	if wc.Worker.PrivateIP != "" {
		return wc.Worker.PrivateIP
	}
	return wc.Worker.IPAddress
}
