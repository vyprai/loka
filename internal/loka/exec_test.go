package loka

import "testing"

func TestExecutionIsTerminal(t *testing.T) {
	tests := []struct {
		status   ExecStatus
		terminal bool
	}{
		{ExecStatusPending, false},
		{ExecStatusPendingApproval, false},
		{ExecStatusRunning, false},
		{ExecStatusSuccess, true},
		{ExecStatusFailed, true},
		{ExecStatusCanceled, true},
		{ExecStatusRejected, true},
	}

	for _, tt := range tests {
		e := &Execution{Status: tt.status}
		if got := e.IsTerminal(); got != tt.terminal {
			t.Errorf("Execution(%s).IsTerminal() = %v, want %v", tt.status, got, tt.terminal)
		}
	}
}
