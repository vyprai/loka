package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
)

type createCheckpointReq struct {
	Type  string `json:"type"`
	Label string `json:"label"`
}

func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	var req createCheckpointReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cpType := loka.CheckpointLight
	if req.Type == "full" {
		cpType = loka.CheckpointFull
	}

	checkpointID := uuid.New().String()

	// Find the current latest checkpoint to set as parent.
	var parentID string
	dag, err := s.store.Checkpoints().GetDAG(r.Context(), sessionID)
	if err == nil && len(dag.Checkpoints) > 0 {
		parentID = findLeaf(dag)
	}

	cp, err := s.sessionManager.CreateCheckpoint(r.Context(), sessionID, checkpointID, cpType, parentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Set the label (the session manager doesn't handle labels).
	if req.Label != "" {
		cp.Label = req.Label
		// Update by delete + recreate.
		s.store.Checkpoints().Delete(r.Context(), cp.ID)
		s.store.Checkpoints().Create(r.Context(), cp)
	}

	// Wait briefly for the checkpoint to complete (worker is async).
	// Poll up to 5 seconds.
	for i := 0; i < 50; i++ {
		updated, err := s.store.Checkpoints().Get(r.Context(), cp.ID)
		if err == nil && updated.Status != loka.CheckpointStatusCreating {
			cp = updated
			// Preserve label.
			if req.Label != "" {
				cp.Label = req.Label
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	s.logger.Info("checkpoint created", "id", cp.ID, "session", sessionID, "type", cpType, "status", cp.Status)
	writeJSON(w, http.StatusCreated, cp)
}

func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	dag, err := s.store.Checkpoints().GetDAG(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cps, _ := s.store.Checkpoints().ListBySession(r.Context(), sessionID)
	if cps == nil {
		cps = []*loka.Checkpoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":  sessionID,
		"root":        dag.Root,
		"current":     dag.Current,
		"checkpoints": cps,
	})
}

func (s *Server) restoreCheckpoint(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	cpID := chi.URLParam(r, "cpId")

	cp, err := s.store.Checkpoints().Get(r.Context(), cpID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if cp.SessionID != sessionID {
		writeError(w, http.StatusBadRequest, "checkpoint does not belong to session")
		return
	}

	if err := s.sessionManager.RestoreCheckpoint(r.Context(), sessionID, cpID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Wait briefly for restore to complete.
	time.Sleep(200 * time.Millisecond)

	sess, err := s.sessionManager.Get(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) deleteCheckpoint(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	cpID := chi.URLParam(r, "cpId")

	cp, err := s.store.Checkpoints().Get(r.Context(), cpID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if cp.SessionID != sessionID {
		writeError(w, http.StatusBadRequest, "checkpoint does not belong to session")
		return
	}

	if err := s.store.Checkpoints().DeleteSubtree(r.Context(), cpID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) diffCheckpoints(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	cpA := r.URL.Query().Get("a")
	cpB := r.URL.Query().Get("b")

	if cpA == "" || cpB == "" {
		writeError(w, http.StatusBadRequest, "query params 'a' and 'b' (checkpoint IDs) are required")
		return
	}

	// Verify both checkpoints belong to this session.
	a, err := s.store.Checkpoints().Get(r.Context(), cpA)
	if err != nil {
		writeError(w, http.StatusNotFound, "checkpoint a: "+err.Error())
		return
	}
	b, err := s.store.Checkpoints().Get(r.Context(), cpB)
	if err != nil {
		writeError(w, http.StatusNotFound, "checkpoint b: "+err.Error())
		return
	}
	if a.SessionID != sessionID || b.SessionID != sessionID {
		writeError(w, http.StatusBadRequest, "checkpoints must belong to this session")
		return
	}

	// The diff is computed by the worker. For now, return the checkpoint
	// overlay paths so the caller knows what to compare.
	// In production, the worker would compute the diff from the overlay layers.
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":    sessionID,
		"checkpoint_a":  cpA,
		"checkpoint_b":  cpB,
		"overlay_a":     a.OverlayPath,
		"overlay_b":     b.OverlayPath,
		"label_a":       a.Label,
		"label_b":       b.Label,
		"type_a":        a.Type,
		"type_b":        b.Type,
	})
}

// findLeaf finds the most recent leaf checkpoint in the DAG.
func findLeaf(dag *loka.CheckpointDAG) string {
	if len(dag.Checkpoints) == 0 {
		return ""
	}
	hasChild := make(map[string]bool)
	for _, cp := range dag.Checkpoints {
		if cp.ParentID != "" {
			hasChild[cp.ParentID] = true
		}
	}
	var latest *loka.Checkpoint
	for _, cp := range dag.Checkpoints {
		if !hasChild[cp.ID] {
			if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
				latest = cp
			}
		}
	}
	if latest != nil {
		return latest.ID
	}
	return ""
}
