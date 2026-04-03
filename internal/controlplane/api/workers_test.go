package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/vyprai/loka/internal/loka"
)

func TestLabelWorker(t *testing.T) {
	ts := setupTestServer(t)
	w := ts.registerTestWorker(t)

	payload := map[string]any{
		"labels": map[string]string{
			"region": "us-west-2",
			"tier":   "gpu",
		},
	}
	rec := ts.doRequest(t, http.MethodPut, "/api/v1/workers/"+w.ID+"/labels", payload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated loka.Worker
	decodeBody(t, rec, &updated)
	if updated.Labels["region"] != "us-west-2" {
		t.Errorf("expected label region=us-west-2, got %q", updated.Labels["region"])
	}
	if updated.Labels["tier"] != "gpu" {
		t.Errorf("expected label tier=gpu, got %q", updated.Labels["tier"])
	}

	// Verify the original "env" label from registerTestWorker is preserved.
	if updated.Labels["env"] != "test" {
		t.Errorf("expected original label env=test preserved, got %q", updated.Labels["env"])
	}
}

func TestLabelWorker_RemoveLabel(t *testing.T) {
	ts := setupTestServer(t)
	w := ts.registerTestWorker(t)

	// Remove the "env" label by setting it to empty.
	payload := map[string]any{
		"labels": map[string]string{
			"env": "",
		},
	}
	rec := ts.doRequest(t, http.MethodPut, "/api/v1/workers/"+w.ID+"/labels", payload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated loka.Worker
	decodeBody(t, rec, &updated)
	if _, exists := updated.Labels["env"]; exists {
		t.Errorf("expected label env to be removed, but it still exists: %v", updated.Labels)
	}
}

func TestLabelWorker_NotFound(t *testing.T) {
	ts := setupTestServer(t)

	payload := map[string]any{
		"labels": map[string]string{"foo": "bar"},
	}
	rec := ts.doRequest(t, http.MethodPut, "/api/v1/workers/nonexistent/labels", payload, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDrainWorker(t *testing.T) {
	ts := setupTestServer(t)
	w := ts.registerTestWorker(t)

	payload := map[string]any{
		"timeout_seconds": 60,
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/drain", payload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated loka.Worker
	decodeBody(t, rec, &updated)
	if updated.Status != loka.WorkerStatusDraining {
		t.Errorf("expected status draining, got %q", updated.Status)
	}
}

func TestUndrainWorker(t *testing.T) {
	ts := setupTestServer(t)
	w := ts.registerTestWorker(t)

	// First drain the worker.
	drainPayload := map[string]any{"timeout_seconds": 60}
	drainRec := ts.doRequest(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/drain", drainPayload, nil)
	if drainRec.Code != http.StatusOK {
		t.Fatalf("drain: expected 200, got %d: %s", drainRec.Code, drainRec.Body.String())
	}

	// Then undrain it.
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/undrain", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated loka.Worker
	decodeBody(t, rec, &updated)
	if updated.Status != loka.WorkerStatusReady {
		t.Errorf("expected status ready after undrain, got %q", updated.Status)
	}
}

func TestRemoveWorker(t *testing.T) {
	ts := setupTestServer(t)
	w := ts.registerTestWorker(t)

	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify worker is gone from the store.
	_, err := ts.store.Workers().Get(context.Background(), w.ID)
	if err == nil {
		t.Error("expected error fetching removed worker, got nil")
	}
}

func TestRemoveWorker_NotFound(t *testing.T) {
	ts := setupTestServer(t)

	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/workers/nonexistent", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}
