package loka

import (
	"fmt"
	"testing"
)

func TestServiceCanTransitionTo(t *testing.T) {
	tests := []struct {
		from   ServiceStatus
		to     ServiceStatus
		expect bool
	}{
		// Valid transitions.
		{ServiceStatusDeploying, ServiceStatusRunning, true},
		{ServiceStatusDeploying, ServiceStatusError, true},
		{ServiceStatusRunning, ServiceStatusIdle, true},
		{ServiceStatusRunning, ServiceStatusStopped, true},
		{ServiceStatusRunning, ServiceStatusError, true},
		{ServiceStatusIdle, ServiceStatusWaking, true},
		{ServiceStatusIdle, ServiceStatusStopped, true},
		{ServiceStatusIdle, ServiceStatusError, true},
		{ServiceStatusWaking, ServiceStatusRunning, true},
		{ServiceStatusWaking, ServiceStatusError, true},
		{ServiceStatusStopped, ServiceStatusDeploying, true},
		{ServiceStatusStopped, ServiceStatusError, true},
		{ServiceStatusError, ServiceStatusDeploying, true},
		{ServiceStatusError, ServiceStatusStopped, true},

		// Invalid transitions.
		{ServiceStatusRunning, ServiceStatusDeploying, false},
		{ServiceStatusIdle, ServiceStatusRunning, false},
		{ServiceStatusStopped, ServiceStatusRunning, false},
		{ServiceStatusDeploying, ServiceStatusIdle, false},
		{ServiceStatusDeploying, ServiceStatusStopped, false},
		{ServiceStatusDeploying, ServiceStatusWaking, false},
		{ServiceStatusWaking, ServiceStatusIdle, false},
		{ServiceStatusWaking, ServiceStatusStopped, false},
		{ServiceStatusWaking, ServiceStatusDeploying, false},
		{ServiceStatusError, ServiceStatusRunning, false},
		{ServiceStatusError, ServiceStatusIdle, false},
		{ServiceStatusError, ServiceStatusWaking, false},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s->%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			s := &Service{Status: tt.from}
			got := s.CanTransitionTo(tt.to)
			if got != tt.expect {
				t.Errorf("Service(%s).CanTransitionTo(%s) = %v, want %v", tt.from, tt.to, got, tt.expect)
			}
		})
	}
}

// TestServiceCanTransitionTo_ExhaustiveValid tests every valid transition
// defined in the state machine to ensure none are accidentally removed.
func TestServiceCanTransitionTo_ExhaustiveValid(t *testing.T) {
	for from, targets := range ValidServiceTransitions {
		for _, to := range targets {
			name := fmt.Sprintf("%s->%s", from, to)
			t.Run(name, func(t *testing.T) {
				s := &Service{Status: from}
				if !s.CanTransitionTo(to) {
					t.Errorf("expected %s -> %s to be valid, but CanTransitionTo returned false", from, to)
				}
			})
		}
	}
}

// TestServiceCanTransitionTo_ExhaustiveInvalid tests transitions that must be rejected.
func TestServiceCanTransitionTo_ExhaustiveInvalid(t *testing.T) {
	invalid := []struct {
		from ServiceStatus
		to   ServiceStatus
	}{
		// deploying: cannot go to idle, waking, stopped directly
		{ServiceStatusDeploying, ServiceStatusIdle},
		{ServiceStatusDeploying, ServiceStatusWaking},
		{ServiceStatusDeploying, ServiceStatusStopped},
		// running: cannot go back to deploying or waking
		{ServiceStatusRunning, ServiceStatusDeploying},
		{ServiceStatusRunning, ServiceStatusWaking},
		// idle: cannot go to running or deploying directly
		{ServiceStatusIdle, ServiceStatusRunning},
		{ServiceStatusIdle, ServiceStatusDeploying},
		// waking: cannot go to idle, stopped, or deploying
		{ServiceStatusWaking, ServiceStatusIdle},
		{ServiceStatusWaking, ServiceStatusStopped},
		{ServiceStatusWaking, ServiceStatusDeploying},
		// stopped: cannot go to running, idle, or waking
		{ServiceStatusStopped, ServiceStatusRunning},
		{ServiceStatusStopped, ServiceStatusIdle},
		{ServiceStatusStopped, ServiceStatusWaking},
		// error: limited transitions
		{ServiceStatusError, ServiceStatusRunning},
		{ServiceStatusError, ServiceStatusIdle},
		{ServiceStatusError, ServiceStatusWaking},
	}

	for _, tt := range invalid {
		name := fmt.Sprintf("%s->%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			s := &Service{Status: tt.from}
			if s.CanTransitionTo(tt.to) {
				t.Errorf("expected %s -> %s to be invalid, but CanTransitionTo returned true", tt.from, tt.to)
			}
		})
	}
}

// TestServiceCanTransitionTo_SelfTransition ensures no status can transition to itself.
func TestServiceCanTransitionTo_SelfTransition(t *testing.T) {
	allStatuses := []ServiceStatus{
		ServiceStatusDeploying, ServiceStatusRunning, ServiceStatusIdle,
		ServiceStatusWaking, ServiceStatusStopped, ServiceStatusError,
	}
	for _, status := range allStatuses {
		t.Run(string(status), func(t *testing.T) {
			s := &Service{Status: status}
			if s.CanTransitionTo(status) {
				t.Errorf("status %s should not be able to transition to itself", status)
			}
		})
	}
}

// TestServiceCanTransitionTo_UnknownStatus verifies that an unrecognized status
// cannot transition to anything.
func TestServiceCanTransitionTo_UnknownStatus(t *testing.T) {
	s := &Service{Status: ServiceStatus("unknown")}
	allStatuses := []ServiceStatus{
		ServiceStatusDeploying, ServiceStatusRunning, ServiceStatusIdle,
		ServiceStatusWaking, ServiceStatusStopped, ServiceStatusError,
	}
	for _, target := range allStatuses {
		if s.CanTransitionTo(target) {
			t.Errorf("unknown status should not transition to %s", target)
		}
	}
}
