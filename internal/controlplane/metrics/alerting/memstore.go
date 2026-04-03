package alerting

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemStore is an in-memory implementation of AlertRuleStore for development/testing.
type MemStore struct {
	mu             sync.RWMutex
	alertRules     map[string]AlertRule
	alerts         map[string]Alert
	recordingRules map[string]RecordingRule
}

// NewMemStore creates a new in-memory alert rule store.
func NewMemStore() *MemStore {
	return &MemStore{
		alertRules:     make(map[string]AlertRule),
		alerts:         make(map[string]Alert),
		recordingRules: make(map[string]RecordingRule),
	}
}

func (m *MemStore) ListAlertRules(ctx context.Context) ([]AlertRule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rules := make([]AlertRule, 0, len(m.alertRules))
	for _, r := range m.alertRules {
		rules = append(rules, r)
	}
	return rules, nil
}

func (m *MemStore) GetAlertRule(ctx context.Context, id string) (*AlertRule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.alertRules[id]
	if !ok {
		return nil, fmt.Errorf("alert rule not found: %s", id)
	}
	return &r, nil
}

func (m *MemStore) CreateAlertRule(ctx context.Context, rule *AlertRule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alertRules[rule.ID] = *rule
	return nil
}

func (m *MemStore) UpdateAlertRule(ctx context.Context, rule *AlertRule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.alertRules[rule.ID]; !ok {
		return fmt.Errorf("alert rule not found: %s", rule.ID)
	}
	m.alertRules[rule.ID] = *rule
	return nil
}

func (m *MemStore) DeleteAlertRule(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.alertRules, id)
	return nil
}

func (m *MemStore) ListAlerts(ctx context.Context, status *AlertStatus, limit int) ([]Alert, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	alerts := make([]Alert, 0)
	for _, a := range m.alerts {
		if status != nil && a.Status != *status {
			continue
		}
		alerts = append(alerts, a)
		if limit > 0 && len(alerts) >= limit {
			break
		}
	}
	return alerts, nil
}

func (m *MemStore) CreateAlert(ctx context.Context, alert *Alert) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts[alert.ID] = *alert
	return nil
}

func (m *MemStore) UpdateAlert(ctx context.Context, alert *Alert) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts[alert.ID] = *alert
	return nil
}

func (m *MemStore) DismissAlert(ctx context.Context, id, dismissedBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.alerts[id]
	if !ok {
		return fmt.Errorf("alert not found: %s", id)
	}
	a.Status = AlertDismissed
	now := time.Now()
	a.DismissedAt = &now
	a.DismissedBy = dismissedBy
	m.alerts[id] = a
	return nil
}

func (m *MemStore) ListRecordingRules(ctx context.Context) ([]RecordingRule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rules := make([]RecordingRule, 0, len(m.recordingRules))
	for _, r := range m.recordingRules {
		rules = append(rules, r)
	}
	return rules, nil
}

func (m *MemStore) CreateRecordingRule(ctx context.Context, rule *RecordingRule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordingRules[rule.ID] = *rule
	return nil
}

func (m *MemStore) DeleteRecordingRule(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recordingRules, id)
	return nil
}
