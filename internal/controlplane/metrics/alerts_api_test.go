package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/metrics/alerting"
)

func newTestAPI() (*AlertsAPI, chi.Router) {
	store := alerting.NewMemStore()
	api := &AlertsAPI{Store: store}
	r := chi.NewRouter()
	r.Mount("/alerts", api.Routes())
	return api, r
}

type apiResponse struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

func TestCreateAlertRule(t *testing.T) {
	_, router := newTestAPI()

	body := `{"name":"high-cpu","query":"cpu_usage","condition":">","threshold":90}`
	req := httptest.NewRequest(http.MethodPost, "/alerts/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}

	var rule alerting.AlertRule
	if err := json.Unmarshal(resp.Data, &rule); err != nil {
		t.Fatalf("bad rule json: %v", err)
	}
	if rule.Name != "high-cpu" {
		t.Errorf("expected name high-cpu, got %s", rule.Name)
	}
	if rule.ID == "" {
		t.Error("expected auto-generated ID")
	}
	if !rule.Enabled {
		t.Error("expected rule to be enabled by default")
	}
}

func TestListAlertRules(t *testing.T) {
	_, router := newTestAPI()

	for _, name := range []string{"rule-a", "rule-b"} {
		body := `{"name":"` + name + `","query":"q","condition":">"}`
		req := httptest.NewRequest(http.MethodPost, "/alerts/rules", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("create failed for %s: %d", name, w.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/alerts/rules", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp apiResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	var rules []alerting.AlertRule
	json.Unmarshal(resp.Data, &rules)
	if len(rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(rules))
	}
}

func TestUpdateAlertRule(t *testing.T) {
	api, router := newTestAPI()

	// Create a rule directly in the store.
	rule := &alerting.AlertRule{ID: "r1", Name: "original", Query: "q", Condition: ">"}
	api.Store.CreateAlertRule(context.Background(), rule)

	body := `{"name":"updated","query":"q2","condition":"<","threshold":50}`
	req := httptest.NewRequest(http.MethodPut, "/alerts/rules/r1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp apiResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	var updated alerting.AlertRule
	json.Unmarshal(resp.Data, &updated)

	if updated.Name != "updated" {
		t.Errorf("expected name updated, got %s", updated.Name)
	}
	if updated.ID != "r1" {
		t.Errorf("expected ID r1 preserved, got %s", updated.ID)
	}
}

func TestDeleteAlertRule(t *testing.T) {
	api, router := newTestAPI()

	rule := &alerting.AlertRule{ID: "del1", Name: "todelete", Query: "q", Condition: ">"}
	api.Store.CreateAlertRule(context.Background(), rule)

	req := httptest.NewRequest(http.MethodDelete, "/alerts/rules/del1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify it's gone.
	rules, _ := api.Store.ListAlertRules(context.Background())
	if len(rules) != 0 {
		t.Errorf("expected 0 rules after delete, got %d", len(rules))
	}
}

func TestListAlerts(t *testing.T) {
	api, router := newTestAPI()

	firing := alerting.AlertFiring
	resolved := alerting.AlertResolved

	api.Store.CreateAlert(context.Background(), &alerting.Alert{ID: "a1", Status: firing, RuleName: "r1"})
	api.Store.CreateAlert(context.Background(), &alerting.Alert{ID: "a2", Status: resolved, RuleName: "r2"})
	api.Store.CreateAlert(context.Background(), &alerting.Alert{ID: "a3", Status: firing, RuleName: "r3"})

	// Filter by status=firing
	req := httptest.NewRequest(http.MethodGet, "/alerts/?status=firing", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp apiResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	var alerts []alerting.Alert
	json.Unmarshal(resp.Data, &alerts)

	for _, a := range alerts {
		if a.Status != firing {
			t.Errorf("expected all alerts to be firing, got %s for %s", a.Status, a.ID)
		}
	}
	if len(alerts) != 2 {
		t.Errorf("expected 2 firing alerts, got %d", len(alerts))
	}
}

func TestDismissAlert(t *testing.T) {
	api, router := newTestAPI()

	api.Store.CreateAlert(context.Background(), &alerting.Alert{
		ID:       "dismiss1",
		Status:   alerting.AlertFiring,
		RuleName: "test",
	})

	body := `{"dismissed_by":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/alerts/dismiss1/dismiss", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify status changed.
	dismissed := alerting.AlertDismissed
	alerts, _ := api.Store.ListAlerts(context.Background(), &dismissed, 10)
	found := false
	for _, a := range alerts {
		if a.ID == "dismiss1" {
			found = true
			if a.DismissedBy != "admin" {
				t.Errorf("expected dismissed_by admin, got %s", a.DismissedBy)
			}
		}
	}
	if !found {
		t.Error("dismissed alert not found")
	}
}

func TestListRecordingRules(t *testing.T) {
	api, router := newTestAPI()

	api.Store.CreateRecordingRule(context.Background(), &alerting.RecordingRule{
		ID: "rr1", Name: "avg_cpu", Query: "avg(cpu)", Interval: "1m",
	})
	api.Store.CreateRecordingRule(context.Background(), &alerting.RecordingRule{
		ID: "rr2", Name: "max_mem", Query: "max(mem)", Interval: "5m",
	})

	req := httptest.NewRequest(http.MethodGet, "/alerts/rules/recording", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp apiResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	var rules []alerting.RecordingRule
	json.Unmarshal(resp.Data, &rules)
	if len(rules) != 2 {
		t.Errorf("expected 2 recording rules, got %d", len(rules))
	}
}

func TestCreateRecordingRule(t *testing.T) {
	_, router := newTestAPI()

	body := `{"name":"p99_latency","query":"histogram_quantile(0.99, rate(http_duration_bucket[5m]))","interval":"30s"}`
	req := httptest.NewRequest(http.MethodPost, "/alerts/rules/recording", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp apiResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	var rule alerting.RecordingRule
	json.Unmarshal(resp.Data, &rule)

	if rule.Name != "p99_latency" {
		t.Errorf("expected name p99_latency, got %s", rule.Name)
	}
	if rule.ID == "" {
		t.Error("expected auto-generated ID")
	}
}
