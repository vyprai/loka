package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/provider"
)

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	names := s.providerRegistry.List()
	var providers []map[string]string
	for _, name := range names {
		providers = append(providers, map[string]string{"name": name})
	}
	if providers == nil {
		providers = []map[string]string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

type provisionReq struct {
	InstanceType string            `json:"instance_type"`
	Region       string            `json:"region"`
	Zone         string            `json:"zone"`
	Count        int               `json:"count"`
	Labels       map[string]string `json:"labels"`
}

func (s *Server) provisionWorkers(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "provider")
	var req provisionReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	p, ok := s.providerRegistry.Get(providerName)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found: "+providerName)
		return
	}

	workers, err := p.Provision(r.Context(), provider.ProvisionOpts{
		InstanceType: req.InstanceType,
		Region:       req.Region,
		Zone:         req.Zone,
		Count:        req.Count,
		Labels:       req.Labels,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workers": workers})
}

func (s *Server) deprovisionWorker(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "provider")
	workerID := chi.URLParam(r, "workerId")

	p, ok := s.providerRegistry.Get(providerName)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found: "+providerName)
		return
	}

	if err := p.Deprovision(r.Context(), workerID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) providerStatus(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "provider")
	p, ok := s.providerRegistry.Get(providerName)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found: "+providerName)
		return
	}

	workers, err := p.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if workers == nil {
		workers = []*provider.WorkerInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": providerName,
		"workers":  workers,
		"total":    len(workers),
	})
}

// ─── Worker Tokens ──────────────────────────────────────

type createTokenReq struct {
	Name           string `json:"name"`
	ExpiresSeconds int    `json:"expires_seconds"`
}

func (s *Server) createWorkerToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" {
		req.Name = "unnamed"
	}

	expiresAt := time.Time{}
	if req.ExpiresSeconds > 0 {
		expiresAt = time.Now().Add(time.Duration(req.ExpiresSeconds) * time.Second)
	}

	token := &loka.WorkerToken{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Token:     loka.GenerateToken(),
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
	}

	if err := s.store.Tokens().Create(r.Context(), token); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, token)
}

func (s *Server) listWorkerTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.Tokens().List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tokens == nil {
		tokens = []*loka.WorkerToken{}
	}
	// Mask token values in list.
	type maskedToken struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
		Used      bool      `json:"used"`
		WorkerID  string    `json:"worker_id"`
		CreatedAt time.Time `json:"created_at"`
	}
	var masked []maskedToken
	for _, t := range tokens {
		tok := t.Token
		if len(tok) > 12 {
			tok = tok[:12] + "..."
		}
		masked = append(masked, maskedToken{
			ID: t.ID, Name: t.Name, Token: tok,
			ExpiresAt: t.ExpiresAt, Used: t.Used,
			WorkerID: t.WorkerID, CreatedAt: t.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": masked, "total": len(masked)})
}

func (s *Server) revokeWorkerToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "tokenId")
	if err := s.store.Tokens().Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
