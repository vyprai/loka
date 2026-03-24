package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type deployServiceReq struct {
	Name           string                `json:"name"`
	Image          string                `json:"image"`
	RecipeName     string                `json:"recipe_name,omitempty"`
	Command        string                `json:"command,omitempty"`
	Args           []string              `json:"args,omitempty"`
	Env            map[string]string     `json:"env,omitempty"`
	Workdir        string                `json:"workdir,omitempty"`
	Port           int                   `json:"port,omitempty"`
	VCPUs          int                   `json:"vcpus,omitempty"`
	MemoryMB       int                   `json:"memory_mb,omitempty"`
	Routes         []loka.ServiceRoute   `json:"routes,omitempty"`
	BundleKey      string                `json:"bundle_key,omitempty"`
	IdleTimeout    int                   `json:"idle_timeout,omitempty"`
	HealthPath     string                `json:"health_path,omitempty"`
	HealthInterval int                   `json:"health_interval,omitempty"`
	HealthTimeout  int                   `json:"health_timeout,omitempty"`
	HealthRetries  int                   `json:"health_retries,omitempty"`
	Labels         map[string]string     `json:"labels,omitempty"`
	Mounts         []loka.VolumeMount    `json:"mounts,omitempty"`
	Autoscale      *loka.AutoscaleConfig `json:"autoscale,omitempty"`
}

func (s *Server) deployService(w http.ResponseWriter, r *http.Request) {
	var req deployServiceReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	svc, err := s.serviceManager.Deploy(r.Context(), service.DeployOpts{
		Name:           req.Name,
		ImageRef:       req.Image,
		RecipeName:     req.RecipeName,
		Command:        req.Command,
		Args:           req.Args,
		Env:            req.Env,
		Workdir:        req.Workdir,
		Port:           req.Port,
		VCPUs:          req.VCPUs,
		MemoryMB:       req.MemoryMB,
		Routes:         req.Routes,
		BundleKey:      req.BundleKey,
		IdleTimeout:    req.IdleTimeout,
		HealthPath:     req.HealthPath,
		HealthInterval: req.HealthInterval,
		HealthTimeout:  req.HealthTimeout,
		HealthRetries:  req.HealthRetries,
		Labels:         req.Labels,
		Mounts:         req.Mounts,
		Autoscale:      req.Autoscale,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// If ?wait=true, block until the service is ready.
	if r.URL.Query().Get("wait") == "true" && !svc.Ready {
		svc, err = s.serviceManager.WaitForReady(r.Context(), svc.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, svc)
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.serviceManager.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	var filter store.ServiceFilter
	if status := r.URL.Query().Get("status"); status != "" {
		st := loka.ServiceStatus(status)
		filter.Status = &st
	}
	if workerID := r.URL.Query().Get("worker_id"); workerID != "" {
		filter.WorkerID = &workerID
	}
	if name := r.URL.Query().Get("name"); name != "" {
		filter.Name = &name
	}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil {
			filter.Limit = v
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if v, err := strconv.Atoi(offsetStr); err == nil {
			filter.Offset = v
		}
	}

	services, total, err := s.serviceManager.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if services == nil {
		services = []*loka.Service{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"services": services,
		"total":    total,
	})
}

func (s *Server) destroyService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.serviceManager.Destroy(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) stopService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.serviceManager.Stop(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) redeployService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.serviceManager.Redeploy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// If ?wait=true, block until the service is ready.
	if r.URL.Query().Get("wait") == "true" && !svc.Ready {
		svc, err = s.serviceManager.WaitForReady(r.Context(), svc.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, svc)
}

type updateServiceEnvReq struct {
	Env map[string]string `json:"env"`
}

func (s *Server) updateServiceEnv(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req updateServiceEnvReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	svc, err := s.serviceManager.UpdateEnv(r.Context(), id, req.Env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) getServiceLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	lines := 100
	if linesStr := r.URL.Query().Get("lines"); linesStr != "" {
		if v, err := strconv.Atoi(linesStr); err == nil && v > 0 {
			lines = v
		}
	}

	stdout, stderr, err := s.serviceManager.Logs(r.Context(), id, lines)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout": stdout,
		"stderr": stderr,
	})
}
