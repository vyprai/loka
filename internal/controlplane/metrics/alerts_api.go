package metrics

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/metrics/alerting"
)

// AlertsAPI provides REST handlers for alert rules and alerts.
type AlertsAPI struct {
	Store   alerting.AlertRuleStore
	Manager *alerting.AlertManager
}

// Routes returns a chi sub-router with all alerts API endpoints mounted.
func (api *AlertsAPI) Routes() chi.Router {
	r := chi.NewRouter()

	// Alert rules CRUD.
	r.Get("/rules", api.handleListRules)
	r.Post("/rules", api.handleCreateRule)
	r.Put("/rules/{id}", api.handleUpdateRule)
	r.Delete("/rules/{id}", api.handleDeleteRule)

	// Recording rules.
	r.Get("/rules/recording", api.handleListRecordingRules)
	r.Post("/rules/recording", api.handleCreateRecordingRule)
	r.Delete("/rules/recording/{id}", api.handleDeleteRecordingRule)

	// Alerts.
	r.Get("/", api.handleListAlerts)
	r.Post("/{id}/dismiss", api.handleDismissAlert)
	r.Get("/history", api.handleAlertHistory)

	return r
}

// handleListRules returns all alert rules.
func (api *AlertsAPI) handleListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := api.Store.ListAlertRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	if rules == nil {
		rules = []alerting.AlertRule{}
	}
	writeSuccess(w, rules)
}

// handleCreateRule creates a new alert rule from the JSON body.
func (api *AlertsAPI) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var rule alerting.AlertRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid JSON body: "+err.Error())
		return
	}

	if rule.Name == "" || rule.Query == "" || rule.Condition == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "name, query, and condition are required")
		return
	}

	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}
	now := time.Now()
	rule.CreatedAt = now
	rule.UpdatedAt = now
	rule.Enabled = true

	if err := api.Store.CreateAlertRule(r.Context(), &rule); err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	writeSuccess(w, rule)
}

// handleUpdateRule updates an existing alert rule.
func (api *AlertsAPI) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	existing, err := api.Store.GetAlertRule(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "alert rule not found: "+err.Error())
		return
	}

	var update alerting.AlertRule
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid JSON body: "+err.Error())
		return
	}

	// Preserve immutable fields.
	update.ID = existing.ID
	update.CreatedAt = existing.CreatedAt
	update.UpdatedAt = time.Now()

	if err := api.Store.UpdateAlertRule(r.Context(), &update); err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	writeSuccess(w, update)
}

// handleDeleteRule deletes an alert rule by ID.
func (api *AlertsAPI) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := api.Store.DeleteAlertRule(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	writeSuccess(w, map[string]string{"deleted": id})
}

// handleListAlerts returns active and recent alerts, filterable by status and limit.
func (api *AlertsAPI) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	var statusFilter *alerting.AlertStatus
	if s := r.URL.Query().Get("status"); s != "" {
		st := alerting.AlertStatus(s)
		statusFilter = &st
	}

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	alerts, err := api.Store.ListAlerts(r.Context(), statusFilter, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	if alerts == nil {
		alerts = []alerting.Alert{}
	}
	writeSuccess(w, alerts)
}

// handleDismissAlert dismisses a firing alert.
func (api *AlertsAPI) handleDismissAlert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var body struct {
		DismissedBy string `json:"dismissed_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid JSON body: "+err.Error())
		return
	}
	if body.DismissedBy == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "dismissed_by is required")
		return
	}

	if api.Manager != nil {
		if err := api.Manager.DismissAlert(r.Context(), id, body.DismissedBy); err != nil {
			writeError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
	} else {
		if err := api.Store.DismissAlert(r.Context(), id, body.DismissedBy); err != nil {
			writeError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
	}

	writeSuccess(w, map[string]string{"dismissed": id})
}

// handleAlertHistory returns alert history with pagination.
func (api *AlertsAPI) handleAlertHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	// The "since" parameter is not used for store filtering (the store does
	// not support time-based queries), but we document it for future use and
	// return all alerts up to the limit. The "offset" param is similarly
	// accepted for API completeness.
	_ = r.URL.Query().Get("since")
	_ = r.URL.Query().Get("offset")

	alerts, err := api.Store.ListAlerts(r.Context(), nil, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	if alerts == nil {
		alerts = []alerting.Alert{}
	}
	writeSuccess(w, alerts)
}

// handleListRecordingRules returns all recording rules.
func (api *AlertsAPI) handleListRecordingRules(w http.ResponseWriter, r *http.Request) {
	rules, err := api.Store.ListRecordingRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	if rules == nil {
		rules = []alerting.RecordingRule{}
	}
	writeSuccess(w, rules)
}

// handleCreateRecordingRule creates a new recording rule.
func (api *AlertsAPI) handleCreateRecordingRule(w http.ResponseWriter, r *http.Request) {
	var rule alerting.RecordingRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid JSON body: "+err.Error())
		return
	}

	if rule.Name == "" || rule.Query == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "name and query are required")
		return
	}

	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}

	if err := api.Store.CreateRecordingRule(r.Context(), &rule); err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	writeSuccess(w, rule)
}

// handleDeleteRecordingRule deletes a recording rule by ID.
func (api *AlertsAPI) handleDeleteRecordingRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := api.Store.DeleteRecordingRule(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	writeSuccess(w, map[string]string{"deleted": id})
}
