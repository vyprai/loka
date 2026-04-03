package alerting

import (
	"context"
	"time"
)

// AlertStatus represents the current state of an alert.
type AlertStatus string

const (
	AlertFiring    AlertStatus = "firing"
	AlertResolved  AlertStatus = "resolved"
	AlertDismissed AlertStatus = "dismissed"
)

// AlertRule defines a condition that, when met for a sustained period, fires an alert.
type AlertRule struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Query       string            `json:"query"`
	Condition   string            `json:"condition"`              // ">", "<", ">=", "<=", "==", "!="
	Threshold   float64           `json:"threshold"`
	For         string            `json:"for"`                    // duration string like "5m"
	Severity    string            `json:"severity"`               // "critical", "warning", "info"
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Webhooks    []string          `json:"webhooks,omitempty"`
	QueryType   string            `json:"query_type,omitempty"`   // "metrics" (default) or "logs"
	LogQuery    string            `json:"log_query,omitempty"`    // LogQL query for log-based alerts
	Enabled     bool              `json:"enabled"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Alert represents a fired (or resolved/dismissed) alert instance.
type Alert struct {
	ID          string            `json:"id"`
	RuleID      string            `json:"rule_id"`
	RuleName    string            `json:"rule_name"`
	Status      AlertStatus       `json:"status"`
	Severity    string            `json:"severity"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Value       float64           `json:"value"`
	FiredAt     time.Time         `json:"fired_at"`
	ResolvedAt  *time.Time        `json:"resolved_at,omitempty"`
	DismissedAt *time.Time        `json:"dismissed_at,omitempty"`
	DismissedBy string            `json:"dismissed_by,omitempty"`
}

// RecordingRule pre-computes a query at a fixed interval and stores the result
// as a new time series.
type RecordingRule struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Query    string            `json:"query"`
	Interval string            `json:"interval"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// AlertRuleStore is the persistence interface for alert rules, alerts, and recording rules.
// It will be implemented by the SQL store layer.
type AlertRuleStore interface {
	ListAlertRules(ctx context.Context) ([]AlertRule, error)
	GetAlertRule(ctx context.Context, id string) (*AlertRule, error)
	CreateAlertRule(ctx context.Context, rule *AlertRule) error
	UpdateAlertRule(ctx context.Context, rule *AlertRule) error
	DeleteAlertRule(ctx context.Context, id string) error

	ListAlerts(ctx context.Context, status *AlertStatus, limit int) ([]Alert, error)
	CreateAlert(ctx context.Context, alert *Alert) error
	UpdateAlert(ctx context.Context, alert *Alert) error
	DismissAlert(ctx context.Context, id, dismissedBy string) error

	ListRecordingRules(ctx context.Context) ([]RecordingRule, error)
	CreateRecordingRule(ctx context.Context, rule *RecordingRule) error
	DeleteRecordingRule(ctx context.Context, id string) error
}
