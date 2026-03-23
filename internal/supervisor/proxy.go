package supervisor

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vyprai/loka/internal/loka"
)

// CommandProxy is the in-VM execution gateway. ALL commands flow through it.
//
// The proxy does NOT parse or analyze code. Security comes from controlling
// what the process can ACCESS, not from inspecting what the code SAYS.
//
// What the proxy controls:
//
//  1. BINARY GATE — which executables can run (whitelist/blacklist/approval)
//  2. ENVIRONMENT — PATH, env vars, working directory
//  3. FILESYSTEM — RO/RW mount state (delegated to Sandbox)
//  4. NETWORK — outbound/inbound rules (delegated to Sandbox via iptables)
//  5. SYSCALLS — seccomp profile (delegated to Sandbox)
//  6. IO — stdin/stdout/stderr routing, size limits, logging
//  7. RESOURCES — CPU/memory/time limits per process
//  8. AUDIT — every command attempt is logged with verdict
//
// What the proxy does NOT do:
//  - Parse shell scripts or interpreter code
//  - Regex scan for "dangerous" patterns
//  - Try to outsmart obfuscation
//
// Why: A process running `python3 -c "obfuscated_evil"` is contained because:
//  - If python3 is not in /env/bin, it doesn't exist (binary gate)
//  - If filesystem is RO, it can't write anything (mount)
//  - If network is blocked, it can't exfiltrate (iptables)
//  - If execve is blocked by seccomp, it can't spawn subprocesses (kernel)
//  - If there's a CPU/memory limit, it can't DoS (cgroups)
//
// The proxy decides WHETHER to run a binary. The sandbox decides WHAT IT CAN DO.
type CommandProxy struct {
	policy          loka.ExecPolicy
	mode            loka.ExecMode
	auditLog        []AuditEntry
	approvedOneShot map[string]bool
}

// Verdict is the proxy's decision.
type Verdict string

const (
	VerdictAllowed       Verdict = "allowed"
	VerdictBlocked       Verdict = "blocked"
	VerdictNeedsApproval Verdict = "needs_approval"
)

// ValidationResult is the proxy's output for a command.
type ValidationResult struct {
	Verdict Verdict
	Reason  string
	Command loka.Command
}

// AuditEntry records every command that passes through the proxy.
type AuditEntry struct {
	Timestamp time.Time
	Command   string
	Args      []string
	Verdict   Verdict
	Reason    string
}

// NewCommandProxy creates a new proxy with the given policy.
func NewCommandProxy(policy loka.ExecPolicy, mode loka.ExecMode) *CommandProxy {
	return &CommandProxy{
		policy:          policy,
		mode:            mode,
		approvedOneShot: make(map[string]bool),
	}
}

// ApproveOnce marks a command ID as approved for one-time execution.
func (p *CommandProxy) ApproveOnce(execID string) {
	p.approvedOneShot[execID] = true
}

// AddToWhitelist permanently adds a binary to the allowed list.
func (p *CommandProxy) AddToWhitelist(command string) {
	p.policy.AllowedCommands = append(p.policy.AllowedCommands, command)
}

func (p *CommandProxy) SetMode(mode loka.ExecMode)             { p.mode = mode }
func (p *CommandProxy) SetPolicy(policy loka.ExecPolicy)       { p.policy = policy }
func (p *CommandProxy) GetAuditLog() []AuditEntry              { return p.auditLog }

// ── Core: Validate ──────────────────────────────────────
//
// This is the ONLY decision the proxy makes: should this binary run?
// It checks the binary name against:
//  1. Blacklist (always blocked, no override)
//  2. Mode restrictions (ask mode, blocked mode)
//  3. One-shot approvals (agent approved a specific command)
//  4. Whitelist (explicitly allowed)
//  5. Unknown → needs_approval (if whitelist exists)
//  6. No whitelist → allowed by default

func (p *CommandProxy) Validate(cmd loka.Command) *ValidationResult {
	binary := filepath.Base(cmd.Command)

	// 1. Blacklisted binaries are always blocked. No approval can override.
	if matchesBinary(binary, p.policy.BlockedCommands) {
		return p.verdict(cmd, VerdictBlocked, fmt.Sprintf("binary %q is blacklisted", binary))
	}

	// 2. Mode: ask mode requires approval for everything.
	if mp, ok := p.policy.ModeRestrictions[p.mode]; ok {
		if mp.RequireApproval {
			if cmd.ID != "" && p.approvedOneShot[cmd.ID] {
				delete(p.approvedOneShot, cmd.ID)
				return p.verdict(cmd, VerdictAllowed, "one-shot approved (ask mode)")
			}
			return p.verdict(cmd, VerdictNeedsApproval, fmt.Sprintf("binary %q requires approval in %s mode", binary, p.mode))
		}
		if mp.Blocked {
			return p.verdict(cmd, VerdictBlocked, fmt.Sprintf("execution blocked in %s mode", p.mode))
		}
	}

	// 3. One-shot approval from a previous needs_approval cycle.
	if cmd.ID != "" && p.approvedOneShot[cmd.ID] {
		delete(p.approvedOneShot, cmd.ID)
		return p.verdict(cmd, VerdictAllowed, "one-shot approved")
	}

	// 4. Explicitly whitelisted → allowed.
	if p.isWhitelisted(binary) {
		return p.verdict(cmd, VerdictAllowed, "")
	}

	// 5. Whitelist exists but binary not in it → needs approval.
	if len(p.policy.AllowedCommands) > 0 {
		return p.verdict(cmd, VerdictNeedsApproval, fmt.Sprintf("binary %q is not whitelisted", binary))
	}

	// 6. No whitelist → allow everything not blacklisted.
	return p.verdict(cmd, VerdictAllowed, "")
}

func (p *CommandProxy) isWhitelisted(binary string) bool {
	if matchesBinary(binary, p.policy.AllowedCommands) {
		return true
	}
	if mp, ok := p.policy.ModeRestrictions[p.mode]; ok {
		if matchesBinary(binary, mp.AllowedCommands) {
			return true
		}
	}
	return false
}

func matchesBinary(binary string, patterns []string) bool {
	for _, p := range patterns {
		if p == binary {
			return true
		}
		if matched, _ := filepath.Match(p, binary); matched {
			return true
		}
	}
	return false
}

func (p *CommandProxy) verdict(cmd loka.Command, v Verdict, reason string) *ValidationResult {
	p.auditLog = append(p.auditLog, AuditEntry{
		Timestamp: time.Now(),
		Command:   cmd.Command,
		Args:      cmd.Args,
		Verdict:   v,
		Reason:    reason,
	})
	return &ValidationResult{Verdict: v, Reason: reason, Command: cmd}
}

// ── Process Environment ─────────────────────────────────
//
// The proxy builds the environment for each process. This is how
// it controls what the process can access without parsing code.

// ProcessEnv builds the environment variables for a process.
func (p *CommandProxy) ProcessEnv(cmd loka.Command) []string {
	env := []string{
		// Restricted PATH — only binaries in /env/bin are reachable.
		"PATH=" + p.RestrictedPATH(),
		// HOME inside the sandbox.
		"HOME=/workspace",
		// Disable interpreter features that bypass restrictions.
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONNOUSERSITE=1",
		"NODE_OPTIONS=--max-old-space-size=512",
	}

	// User-specified env vars.
	for k, v := range cmd.Env {
		// Block env vars that could bypass security.
		if isBlockedEnvVar(k) {
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	return env
}

// RestrictedPATH returns the PATH for processes inside the sandbox.
func (p *CommandProxy) RestrictedPATH() string {
	if len(p.policy.AllowedCommands) > 0 {
		return "/env/bin"
	}
	return "/env/bin:/usr/local/bin:/usr/bin:/bin"
}

// isBlockedEnvVar returns true for env vars that could bypass sandbox security.
var blockedEnvVars = map[string]bool{
	"PATH": true, "HOME": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true,
	"PYTHONPATH": true, "PYTHONSTARTUP": true, "PYTHONHOME": true,
	"NODE_PATH": true, "NODE_OPTIONS": true,
	"RUBYLIB": true, "RUBYOPT": true,
	"PERL5LIB": true, "PERL5OPT": true,
	"SHELL": true, "BASH_ENV": true, "ENV": true,
}

func isBlockedEnvVar(key string) bool {
	return blockedEnvVars[strings.ToUpper(key)]
}

// ── IO Control ──────────────────────────────────────────
//
// The proxy wraps process IO streams. This enables:
//  - Size limits on stdout/stderr (prevent memory exhaustion)
//  - Audit logging of all output
//  - Filtering sensitive data from output
//  - Rate limiting IO throughput

// IOLimits defines limits on process IO.
type IOLimits struct {
	MaxStdoutBytes int64 // Max stdout size. 0 = default (10MB).
	MaxStderrBytes int64 // Max stderr size. 0 = default (10MB).
	MaxStdinBytes  int64 // Max stdin size. 0 = default (1MB).
}

// DefaultIOLimits returns sensible IO limits.
func DefaultIOLimits() IOLimits {
	return IOLimits{
		MaxStdoutBytes: 10 * 1024 * 1024, // 10MB
		MaxStderrBytes: 10 * 1024 * 1024, // 10MB
		MaxStdinBytes:  1 * 1024 * 1024,  // 1MB
	}
}

// ── Resource Limits ─────────────────────────────────────
//
// Per-process resource limits, enforced via cgroups inside the VM.

// ResourceLimits for a single process.
type ResourceLimits struct {
	MaxCPUSeconds int   // CPU time limit. 0 = policy default.
	MaxMemoryMB   int   // Memory limit. 0 = policy default.
	MaxDiskMB     int   // Disk write limit. 0 = policy default.
	MaxPIDs       int   // Max child processes. 0 = 1 (no fork).
}

// DefaultResourceLimits returns per-process defaults.
func DefaultResourceLimits() ResourceLimits {
	return ResourceLimits{
		MaxCPUSeconds: 300,  // 5 minutes.
		MaxMemoryMB:   512,  // 512MB.
		MaxDiskMB:     1024, // 1GB.
		MaxPIDs:       1,    // No forking by default.
	}
}
