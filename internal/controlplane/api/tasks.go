package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/task"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

func (s *Server) registerTaskRoutes(r chi.Router) {
	r.Post("/tasks", s.runTask)
	r.Get("/tasks", s.listTasks)
	r.Get("/tasks/{id}", s.getTask)
	r.Post("/tasks/{id}/restart", s.restartTask)
	r.Post("/tasks/{id}/cancel", s.cancelTask)
	r.Delete("/tasks/{id}", s.deleteTask)
	r.Get("/tasks/{id}/logs", s.getTaskLogs)
}

type runTaskReq struct {
	Name     string            `json:"name"`
	Image    string            `json:"image"`
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	Workdir  string            `json:"workdir"`
	VCPUs    int               `json:"vcpus"`
	MemoryMB int               `json:"memory_mb"`
	Timeout  int               `json:"timeout"`
}

func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	if s.taskManager == nil {
		writeError(w, http.StatusServiceUnavailable, "task manager not available")
		return
	}
	var req runTaskReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}

	t, err := s.taskManager.Run(r.Context(), task.RunOpts{
		Name:     req.Name,
		ImageRef: req.Image,
		Command:  req.Command,
		Args:     req.Args,
		Env:      req.Env,
		Workdir:  req.Workdir,
		VCPUs:    req.VCPUs,
		MemoryMB: req.MemoryMB,
		Timeout:  req.Timeout,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if s.taskManager == nil {
		writeError(w, http.StatusServiceUnavailable, "task manager not available")
		return
	}
	var filter store.TaskFilter
	if status := r.URL.Query().Get("status"); status != "" {
		st := loka.TaskStatus(status)
		filter.Status = &st
	}
	tasks, err := s.taskManager.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []*loka.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "total": len(tasks)})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if s.taskManager == nil {
		writeError(w, http.StatusServiceUnavailable, "task manager not available")
		return
	}
	id := chi.URLParam(r, "id")
	t, err := s.taskManager.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) restartTask(w http.ResponseWriter, r *http.Request) {
	if s.taskManager == nil {
		writeError(w, http.StatusServiceUnavailable, "task manager not available")
		return
	}
	id := chi.URLParam(r, "id")
	t, err := s.taskManager.Restart(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	if s.taskManager == nil {
		writeError(w, http.StatusServiceUnavailable, "task manager not available")
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.taskManager.Cancel(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	if s.taskManager == nil {
		writeError(w, http.StatusServiceUnavailable, "task manager not available")
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.taskManager.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getTaskLogs(w http.ResponseWriter, r *http.Request) {
	// Task logs come from the service logs infrastructure (same VM).
	// Delegate to service logs handler using the task ID as service ID.
	if s.taskManager == nil {
		writeError(w, http.StatusServiceUnavailable, "task manager not available")
		return
	}
	s.getServiceLogs(w, r)
}
