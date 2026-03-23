package main

import (
	"testing"
)

func TestNewShellCmd_CreatesValidCommand(t *testing.T) {
	cmd := newShellCmd()
	if cmd == nil {
		t.Fatal("newShellCmd returned nil")
	}
	if cmd.Use != "shell <session-id>" {
		t.Errorf("Use = %q, want %q", cmd.Use, "shell <session-id>")
	}
	if cmd.Short == "" {
		t.Error("Short description should not be empty")
	}
}

func TestNewShellCmd_HasShellFlag(t *testing.T) {
	cmd := newShellCmd()
	f := cmd.Flags().Lookup("shell")
	if f == nil {
		t.Fatal("expected --shell flag to exist")
	}
	if f.DefValue != "/bin/bash" {
		t.Errorf("shell flag default = %q, want %q", f.DefValue, "/bin/bash")
	}
}

func TestNewShellCmd_HasWorkdirFlag(t *testing.T) {
	cmd := newShellCmd()
	f := cmd.Flags().Lookup("workdir")
	if f == nil {
		t.Fatal("expected --workdir flag to exist")
	}
	if f.DefValue != "" {
		t.Errorf("workdir flag default = %q, want empty", f.DefValue)
	}
}

func TestNewShellCmd_DefaultShellIsBash(t *testing.T) {
	cmd := newShellCmd()
	val, err := cmd.Flags().GetString("shell")
	if err != nil {
		t.Fatalf("GetString(shell): %v", err)
	}
	if val != "/bin/bash" {
		t.Errorf("default shell = %q, want %q", val, "/bin/bash")
	}
}

func TestNewShellCmd_RequiresExactlyOneArg(t *testing.T) {
	cmd := newShellCmd()

	// Zero args should fail validation.
	err := cmd.Args(cmd, []string{})
	if err == nil {
		t.Error("expected error with 0 args")
	}

	// One arg should pass validation.
	err = cmd.Args(cmd, []string{"session-123"})
	if err != nil {
		t.Errorf("expected no error with 1 arg, got: %v", err)
	}

	// Two args should fail validation.
	err = cmd.Args(cmd, []string{"session-123", "extra"})
	if err == nil {
		t.Error("expected error with 2 args")
	}
}
