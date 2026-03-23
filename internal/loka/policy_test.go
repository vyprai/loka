package loka

import "testing"

func TestValidateCommand_AllowedCommands(t *testing.T) {
	policy := ExecPolicy{
		AllowedCommands: []string{"python3", "git", "ls"},
	}

	// Allowed.
	if err := policy.ValidateCommand(Command{Command: "python3"}, ModeExecute); err != nil {
		t.Errorf("python3 should be allowed: %v", err)
	}
	if err := policy.ValidateCommand(Command{Command: "git"}, ModeExecute); err != nil {
		t.Errorf("git should be allowed: %v", err)
	}

	// Blocked.
	if err := policy.ValidateCommand(Command{Command: "rm"}, ModeExecute); err == nil {
		t.Error("rm should be blocked by allowlist")
	}
	if err := policy.ValidateCommand(Command{Command: "curl"}, ModeExecute); err == nil {
		t.Error("curl should be blocked by allowlist")
	}
}

func TestValidateCommand_BlockedCommands(t *testing.T) {
	policy := ExecPolicy{
		BlockedCommands: []string{"rm", "dd", "mkfs*"},
	}

	// Allowed (not blocked).
	if err := policy.ValidateCommand(Command{Command: "ls"}, ModeExecute); err != nil {
		t.Errorf("ls should be allowed: %v", err)
	}

	// Blocked.
	if err := policy.ValidateCommand(Command{Command: "rm"}, ModeExecute); err == nil {
		t.Error("rm should be blocked")
	}
	if err := policy.ValidateCommand(Command{Command: "dd"}, ModeExecute); err == nil {
		t.Error("dd should be blocked")
	}
}

func TestValidateCommand_GlobPatterns(t *testing.T) {
	policy := ExecPolicy{
		AllowedCommands: []string{"python*", "git"},
	}

	if err := policy.ValidateCommand(Command{Command: "python3"}, ModeExecute); err != nil {
		t.Errorf("python3 should match python*: %v", err)
	}
	if err := policy.ValidateCommand(Command{Command: "python3.12"}, ModeExecute); err != nil {
		t.Errorf("python3.12 should match python*: %v", err)
	}
	if err := policy.ValidateCommand(Command{Command: "node"}, ModeExecute); err == nil {
		t.Error("node should not match python*")
	}
}

func TestValidateCommand_ModeRestrictions(t *testing.T) {
	policy := DefaultExecPolicy()

	// Explore mode: all commands allowed (filesystem is read-only, enforced by supervisor).
	if err := policy.ValidateCommand(Command{Command: "ls"}, ModeExplore); err != nil {
		t.Errorf("ls should be allowed in explore: %v", err)
	}
	if err := policy.ValidateCommand(Command{Command: "python3"}, ModeExplore); err != nil {
		t.Errorf("python3 should be allowed in explore: %v", err)
	}
	if err := policy.ValidateCommand(Command{Command: "rm"}, ModeExplore); err != nil {
		t.Errorf("rm should be allowed in explore (filesystem is read-only): %v", err)
	}

	// Execute mode: all commands allowed.
	if err := policy.ValidateCommand(Command{Command: "rm"}, ModeExecute); err != nil {
		t.Errorf("rm should be allowed in execute: %v", err)
	}
}

func TestValidateCommand_BlockedMode(t *testing.T) {
	policy := ExecPolicy{
		ModeRestrictions: map[ExecMode]ModeExecPolicy{
			ModeExplore: {Blocked: true},
		},
	}

	if err := policy.ValidateCommand(Command{Command: "ls"}, ModeExplore); err == nil {
		t.Error("all commands should be blocked in inspect mode")
	}
	if err := policy.ValidateCommand(Command{Command: "ls"}, ModeExecute); err != nil {
		t.Errorf("ls should be allowed in execute: %v", err)
	}
}

func TestRequiresApproval(t *testing.T) {
	policy := ExecPolicy{
		ModeRestrictions: map[ExecMode]ModeExecPolicy{
			ModeAsk: {RequireApproval: true},
		},
	}

	if !policy.RequiresApproval(ModeAsk) {
		t.Error("ask mode should require approval")
	}
	if policy.RequiresApproval(ModeExecute) {
		t.Error("execute mode should not require approval")
	}
}

func TestExtractBinary(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"python3", "python3"},
		{"/usr/bin/python3", "python3"},
		{"/usr/local/bin/git", "git"},
		{"sh", "sh"},
	}
	for _, tt := range tests {
		got := extractBinary(tt.input)
		if got != tt.expect {
			t.Errorf("extractBinary(%q) = %q, want %q", tt.input, got, tt.expect)
		}
	}
}
