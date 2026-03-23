package loka

import (
	"fmt"
	"testing"
)

func TestSessionCanTransitionTo(t *testing.T) {
	tests := []struct {
		from   SessionStatus
		to     SessionStatus
		expect bool
	}{
		{SessionStatusCreating, SessionStatusRunning, true},
		{SessionStatusCreating, SessionStatusError, true},
		{SessionStatusCreating, SessionStatusTerminated, false},
		{SessionStatusRunning, SessionStatusPaused, true},
		{SessionStatusRunning, SessionStatusTerminating, true},
		{SessionStatusRunning, SessionStatusCreating, false},
		{SessionStatusPaused, SessionStatusRunning, true},
		{SessionStatusPaused, SessionStatusTerminating, true},
		{SessionStatusPaused, SessionStatusCreating, false},
		{SessionStatusTerminating, SessionStatusTerminated, true},
		{SessionStatusTerminating, SessionStatusRunning, false},
		{SessionStatusTerminated, SessionStatusRunning, false},
		{SessionStatusError, SessionStatusTerminating, true},
		{SessionStatusError, SessionStatusRunning, false},
	}

	for _, tt := range tests {
		s := &Session{Status: tt.from}
		got := s.CanTransitionTo(tt.to)
		if got != tt.expect {
			t.Errorf("Session(%s).CanTransitionTo(%s) = %v, want %v", tt.from, tt.to, got, tt.expect)
		}
	}
}

// TestCanTransitionTo_ExhaustiveValid tests every valid transition defined in
// the state machine to ensure none are accidentally removed.
func TestCanTransitionTo_ExhaustiveValid(t *testing.T) {
	valid := []struct {
		from SessionStatus
		to   SessionStatus
	}{
		// creating ->
		{SessionStatusCreating, SessionStatusRunning},
		{SessionStatusCreating, SessionStatusError},
		// running ->
		{SessionStatusRunning, SessionStatusIdle},
		{SessionStatusRunning, SessionStatusPaused},
		{SessionStatusRunning, SessionStatusTerminating},
		{SessionStatusRunning, SessionStatusError},
		// idle ->
		{SessionStatusIdle, SessionStatusRunning},
		{SessionStatusIdle, SessionStatusTerminating},
		{SessionStatusIdle, SessionStatusError},
		// paused ->
		{SessionStatusPaused, SessionStatusRunning},
		{SessionStatusPaused, SessionStatusTerminating},
		{SessionStatusPaused, SessionStatusError},
		// terminating ->
		{SessionStatusTerminating, SessionStatusTerminated},
		{SessionStatusTerminating, SessionStatusError},
		// error ->
		{SessionStatusError, SessionStatusTerminating},
	}

	for _, tt := range valid {
		name := fmt.Sprintf("%s->%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			s := &Session{Status: tt.from}
			if !s.CanTransitionTo(tt.to) {
				t.Errorf("expected %s -> %s to be valid, but CanTransitionTo returned false", tt.from, tt.to)
			}
		})
	}
}

// TestCanTransitionTo_ExhaustiveInvalid tests transitions that must be rejected.
func TestCanTransitionTo_ExhaustiveInvalid(t *testing.T) {
	invalid := []struct {
		from SessionStatus
		to   SessionStatus
	}{
		// creating: cannot go to paused or idle directly
		{SessionStatusCreating, SessionStatusPaused},
		{SessionStatusCreating, SessionStatusIdle},
		{SessionStatusCreating, SessionStatusTerminating},
		{SessionStatusCreating, SessionStatusTerminated},
		// running: cannot go back to creating
		{SessionStatusRunning, SessionStatusCreating},
		{SessionStatusRunning, SessionStatusTerminated},
		// idle: cannot go to paused directly
		{SessionStatusIdle, SessionStatusPaused},
		{SessionStatusIdle, SessionStatusCreating},
		{SessionStatusIdle, SessionStatusTerminated},
		// paused: cannot go to idle directly
		{SessionStatusPaused, SessionStatusIdle},
		{SessionStatusPaused, SessionStatusCreating},
		{SessionStatusPaused, SessionStatusTerminated},
		// terminating: cannot go back
		{SessionStatusTerminating, SessionStatusCreating},
		{SessionStatusTerminating, SessionStatusRunning},
		{SessionStatusTerminating, SessionStatusIdle},
		{SessionStatusTerminating, SessionStatusPaused},
		// terminated: cannot go anywhere
		{SessionStatusTerminated, SessionStatusCreating},
		{SessionStatusTerminated, SessionStatusRunning},
		{SessionStatusTerminated, SessionStatusIdle},
		{SessionStatusTerminated, SessionStatusPaused},
		{SessionStatusTerminated, SessionStatusTerminating},
		{SessionStatusTerminated, SessionStatusError},
		// error: limited transitions
		{SessionStatusError, SessionStatusCreating},
		{SessionStatusError, SessionStatusRunning},
		{SessionStatusError, SessionStatusIdle},
		{SessionStatusError, SessionStatusPaused},
		{SessionStatusError, SessionStatusTerminated},
	}

	for _, tt := range invalid {
		name := fmt.Sprintf("%s->%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			s := &Session{Status: tt.from}
			if s.CanTransitionTo(tt.to) {
				t.Errorf("expected %s -> %s to be invalid, but CanTransitionTo returned true", tt.from, tt.to)
			}
		})
	}
}

// TestIdleStateInTransitionMap ensures the idle status is present in the
// transition map so sessions can transition out of idle.
func TestIdleStateInTransitionMap(t *testing.T) {
	targets, ok := ValidSessionTransitions[SessionStatusIdle]
	if !ok {
		t.Fatal("SessionStatusIdle is missing from ValidSessionTransitions map")
	}
	if len(targets) == 0 {
		t.Fatal("SessionStatusIdle has no valid transitions")
	}

	// Verify the expected outbound transitions from idle.
	expected := map[SessionStatus]bool{
		SessionStatusRunning:     true,
		SessionStatusTerminating: true,
		SessionStatusError:       true,
	}
	for _, target := range targets {
		if !expected[target] {
			t.Errorf("unexpected transition from idle to %s", target)
		}
		delete(expected, target)
	}
	for remaining := range expected {
		t.Errorf("missing transition from idle to %s", remaining)
	}
}

// TestCanTransitionTo_UnknownStatus verifies that an unrecognized status
// cannot transition to anything.
func TestCanTransitionTo_UnknownStatus(t *testing.T) {
	s := &Session{Status: SessionStatus("unknown")}
	allStatuses := []SessionStatus{
		SessionStatusCreating, SessionStatusRunning, SessionStatusIdle,
		SessionStatusPaused, SessionStatusTerminating, SessionStatusTerminated,
		SessionStatusError,
	}
	for _, target := range allStatuses {
		if s.CanTransitionTo(target) {
			t.Errorf("unknown status should not transition to %s", target)
		}
	}
}

// TestProvisioningTransitions tests transitions involving the provisioning status.
func TestProvisioningTransitions(t *testing.T) {
	tests := []struct {
		from   SessionStatus
		to     SessionStatus
		expect bool
	}{
		// creating → provisioning (valid)
		{SessionStatusCreating, SessionStatusProvisioning, true},
		// provisioning → running (valid)
		{SessionStatusProvisioning, SessionStatusRunning, true},
		// provisioning → error (valid)
		{SessionStatusProvisioning, SessionStatusError, true},
		// provisioning → paused (invalid)
		{SessionStatusProvisioning, SessionStatusPaused, false},
		// provisioning → idle (invalid)
		{SessionStatusProvisioning, SessionStatusIdle, false},
		// provisioning → terminated (invalid)
		{SessionStatusProvisioning, SessionStatusTerminated, false},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s->%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			s := &Session{Status: tt.from}
			got := s.CanTransitionTo(tt.to)
			if got != tt.expect {
				t.Errorf("Session(%s).CanTransitionTo(%s) = %v, want %v", tt.from, tt.to, got, tt.expect)
			}
		})
	}
}

// TestProvisioningInTransitionMap ensures provisioning is present in the
// ValidSessionTransitions map with the correct outbound transitions.
func TestProvisioningInTransitionMap(t *testing.T) {
	targets, ok := ValidSessionTransitions[SessionStatusProvisioning]
	if !ok {
		t.Fatal("SessionStatusProvisioning is missing from ValidSessionTransitions map")
	}
	if len(targets) == 0 {
		t.Fatal("SessionStatusProvisioning has no valid transitions")
	}

	expected := map[SessionStatus]bool{
		SessionStatusRunning: true,
		SessionStatusError:   true,
	}
	for _, target := range targets {
		if !expected[target] {
			t.Errorf("unexpected transition from provisioning to %s", target)
		}
		delete(expected, target)
	}
	for remaining := range expected {
		t.Errorf("missing transition from provisioning to %s", remaining)
	}
}

// TestCanTransitionTo_SelfTransition ensures no status can transition to itself.
func TestCanTransitionTo_SelfTransition(t *testing.T) {
	allStatuses := []SessionStatus{
		SessionStatusCreating, SessionStatusRunning, SessionStatusIdle,
		SessionStatusPaused, SessionStatusTerminating, SessionStatusTerminated,
		SessionStatusError,
	}
	for _, status := range allStatuses {
		s := &Session{Status: status}
		if s.CanTransitionTo(status) {
			t.Errorf("status %s should not be able to transition to itself", status)
		}
	}
}
