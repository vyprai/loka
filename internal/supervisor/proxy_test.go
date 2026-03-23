package supervisor

import (
	"fmt"
	"testing"

	"github.com/vyprai/loka/internal/loka"
)

func newTestProxy(opts ...func(*loka.ExecPolicy)) *CommandProxy {
	policy := loka.ExecPolicy{
		BlockedCommands: []string{"rm", "dd", "mkfs"},
		AllowedCommands: []string{"echo", "ls", "cat", "python3", "grep"},
	}
	for _, opt := range opts {
		opt(&policy)
	}
	return NewCommandProxy(policy, loka.ModeExecute)
}

// ── Binary Gate Tests ───────────────────────────────────

func TestProxy_Whitelisted(t *testing.T) {
	proxy := newTestProxy()
	v := proxy.Validate(loka.Command{Command: "echo", Args: []string{"hello"}})
	if v.Verdict != VerdictAllowed {
		t.Errorf("echo should be allowed, got %s: %s", v.Verdict, v.Reason)
	}
}

func TestProxy_Blacklisted(t *testing.T) {
	proxy := newTestProxy()
	v := proxy.Validate(loka.Command{Command: "rm", Args: []string{"-rf", "/"}})
	if v.Verdict != VerdictBlocked {
		t.Errorf("rm should be blocked, got %s", v.Verdict)
	}
}

func TestProxy_NotWhitelisted_NeedsApproval(t *testing.T) {
	proxy := newTestProxy()
	v := proxy.Validate(loka.Command{Command: "wget"})
	if v.Verdict != VerdictNeedsApproval {
		t.Errorf("wget should need approval, got %s", v.Verdict)
	}
}

func TestProxy_NoWhitelist_AllAllowed(t *testing.T) {
	proxy := NewCommandProxy(loka.ExecPolicy{
		BlockedCommands: []string{"rm"},
	}, loka.ModeExecute)

	v := proxy.Validate(loka.Command{Command: "anything"})
	if v.Verdict != VerdictAllowed {
		t.Errorf("no whitelist should allow all, got %s", v.Verdict)
	}

	v2 := proxy.Validate(loka.Command{Command: "rm"})
	if v2.Verdict != VerdictBlocked {
		t.Errorf("blacklist should still block, got %s", v2.Verdict)
	}
}

func TestProxy_FullPath_ExtractsBinary(t *testing.T) {
	proxy := newTestProxy()
	v := proxy.Validate(loka.Command{Command: "/usr/bin/echo"})
	if v.Verdict != VerdictAllowed {
		t.Errorf("/usr/bin/echo should match 'echo' whitelist, got %s", v.Verdict)
	}
}

func TestProxy_GlobPattern(t *testing.T) {
	proxy := NewCommandProxy(loka.ExecPolicy{
		AllowedCommands: []string{"python*"},
	}, loka.ModeExecute)

	v := proxy.Validate(loka.Command{Command: "python3"})
	if v.Verdict != VerdictAllowed {
		t.Errorf("python3 should match python*, got %s", v.Verdict)
	}
}

// ── One-Shot Approval ───────────────────────────────────

func TestProxy_OneShotApproval(t *testing.T) {
	proxy := newTestProxy()
	cmd := loka.Command{ID: "cmd-1", Command: "wget"}

	// First: needs approval.
	v1 := proxy.Validate(cmd)
	if v1.Verdict != VerdictNeedsApproval {
		t.Fatalf("expected needs_approval, got %s", v1.Verdict)
	}

	// Approve it.
	proxy.ApproveOnce("cmd-1")

	// Second: allowed (consumed).
	v2 := proxy.Validate(cmd)
	if v2.Verdict != VerdictAllowed {
		t.Errorf("should be allowed after approve, got %s", v2.Verdict)
	}

	// Third: needs approval again.
	v3 := proxy.Validate(cmd)
	if v3.Verdict != VerdictNeedsApproval {
		t.Errorf("one-shot consumed, should need approval again, got %s", v3.Verdict)
	}
}

func TestProxy_AddToWhitelist(t *testing.T) {
	proxy := newTestProxy()

	v1 := proxy.Validate(loka.Command{Command: "wget"})
	if v1.Verdict != VerdictNeedsApproval {
		t.Fatalf("expected needs_approval, got %s", v1.Verdict)
	}

	proxy.AddToWhitelist("wget")

	v2 := proxy.Validate(loka.Command{Command: "wget"})
	if v2.Verdict != VerdictAllowed {
		t.Errorf("wget should be allowed after whitelist, got %s", v2.Verdict)
	}
}

// ── Ask Mode ────────────────────────────────────────────

func TestProxy_AskMode_AllNeedApproval(t *testing.T) {
	proxy := NewCommandProxy(loka.ExecPolicy{
		AllowedCommands: []string{"echo"},
		ModeRestrictions: map[loka.ExecMode]loka.ModeExecPolicy{
			loka.ModeAsk: {RequireApproval: true},
		},
	}, loka.ModeAsk)

	// Even whitelisted commands need approval in ask mode.
	v := proxy.Validate(loka.Command{Command: "echo"})
	if v.Verdict != VerdictNeedsApproval {
		t.Errorf("ask mode should require approval even for whitelisted, got %s", v.Verdict)
	}
}

func TestProxy_AskMode_BlacklistStillBlocks(t *testing.T) {
	proxy := NewCommandProxy(loka.ExecPolicy{
		BlockedCommands: []string{"rm"},
		ModeRestrictions: map[loka.ExecMode]loka.ModeExecPolicy{
			loka.ModeAsk: {RequireApproval: true},
		},
	}, loka.ModeAsk)

	v := proxy.Validate(loka.Command{Command: "rm"})
	if v.Verdict != VerdictBlocked {
		t.Errorf("blacklist should override ask mode, got %s", v.Verdict)
	}
}

// ── Blocked Mode ────────────────────────────────────────

func TestProxy_BlockedMode(t *testing.T) {
	proxy := NewCommandProxy(loka.ExecPolicy{
		AllowedCommands: []string{"echo"},
		ModeRestrictions: map[loka.ExecMode]loka.ModeExecPolicy{
			loka.ModeExplore: {Blocked: true},
		},
	}, loka.ModeExplore)

	v := proxy.Validate(loka.Command{Command: "echo"})
	if v.Verdict != VerdictBlocked {
		t.Errorf("blocked mode should block everything, got %s", v.Verdict)
	}
}

// ── Environment Control ─────────────────────────────────

func TestProxy_ProcessEnv_RestrictsPATH(t *testing.T) {
	proxy := newTestProxy()
	env := proxy.ProcessEnv(loka.Command{Command: "echo"})

	var path string
	for _, e := range env {
		if len(e) > 5 && e[:5] == "PATH=" {
			path = e[5:]
		}
	}
	if path != "/env/bin" {
		t.Errorf("PATH should be /env/bin when whitelist exists, got %s", path)
	}
}

func TestProxy_ProcessEnv_BlocksDangerousVars(t *testing.T) {
	proxy := newTestProxy()
	env := proxy.ProcessEnv(loka.Command{
		Command: "echo",
		Env: map[string]string{
			"SAFE":       "ok",
			"LD_PRELOAD": "/evil/lib.so",
			"PYTHONPATH": "/evil",
		},
	})

	envMap := make(map[string]string)
	for _, e := range env {
		parts := splitFirst(e, "=")
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if _, ok := envMap["SAFE"]; !ok {
		t.Error("SAFE env var should be passed through")
	}
	if _, ok := envMap["LD_PRELOAD"]; ok {
		t.Error("LD_PRELOAD should be blocked")
	}
	if _, ok := envMap["PYTHONPATH"]; ok {
		t.Error("PYTHONPATH should be blocked")
	}
}

// ── Audit Log ───────────────────────────────────────────

func TestProxy_AuditLog(t *testing.T) {
	proxy := newTestProxy()

	proxy.Validate(loka.Command{Command: "echo"})
	proxy.Validate(loka.Command{Command: "rm"})
	proxy.Validate(loka.Command{Command: "wget"})

	log := proxy.GetAuditLog()
	if len(log) != 3 {
		t.Fatalf("audit log = %d, want 3", len(log))
	}
	if log[0].Verdict != VerdictAllowed {
		t.Error("echo should be allowed")
	}
	if log[1].Verdict != VerdictBlocked {
		t.Error("rm should be blocked")
	}
	if log[2].Verdict != VerdictNeedsApproval {
		t.Error("wget should need approval")
	}
}

// ── Helpers ─────────────────────────────────────────────

func splitFirst(s, sep string) []string {
	i := 0
	for i < len(s) {
		if s[i:i+len(sep)] == sep {
			return []string{s[:i], s[i+len(sep):]}
		}
		i++
	}
	return []string{s}
}

var _ = fmt.Sprint // ensure import
