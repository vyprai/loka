package loka

import (
	"fmt"
	"path/filepath"

)

// ExecPolicy defines what commands and packages are allowed in a session.
// This is set at session creation time and enforced on every exec call.
type ExecPolicy struct {
	// AllowedCommands is a whitelist of command binaries that can be executed.
	// Supports exact names ("python3", "git") and glob patterns ("python*").
	// Empty = all commands allowed.
	AllowedCommands []string `json:"allowed_commands,omitempty"`

	// BlockedCommands is a blacklist of commands that are never allowed.
	// Evaluated after AllowedCommands. Supports exact names and globs.
	BlockedCommands []string `json:"blocked_commands,omitempty"`

	// ModeRestrictions defines per-mode command restrictions.
	// Key is the mode name, value is the policy for that mode.
	ModeRestrictions map[ExecMode]ModeExecPolicy `json:"mode_restrictions,omitempty"`

	// MaxParallel is the maximum number of parallel commands per exec call.
	// 0 = unlimited.
	MaxParallel int `json:"max_parallel,omitempty"`

	// MaxDurationSeconds is the maximum execution time per command.
	// 0 = unlimited.
	MaxDurationSeconds int `json:"max_duration_seconds,omitempty"`

	// NetworkPolicy defines inbound/outbound network access rules.
	// If nil, defaults are applied per execution mode.
	NetworkPolicy *NetworkPolicy `json:"network_policy,omitempty"`

	// FilesystemPolicy defines file/directory access rules.
	// If nil, defaults are applied (workspace RW, system RO, /dev blocked).
	FilesystemPolicy *FilesystemPolicy `json:"filesystem_policy,omitempty"`
}

// ModeExecPolicy defines command restrictions specific to an execution mode.
type ModeExecPolicy struct {
	// AllowedCommands overrides the session-level allowlist for this mode.
	// Empty = inherit from session policy.
	AllowedCommands []string `json:"allowed_commands,omitempty"`

	// ReadOnly when true, only allows commands known to be read-only.
	// This is enforced by checking against ReadOnlyCommands.
	ReadOnly bool `json:"read_only,omitempty"`

	// RequireApproval when true, exec returns pending_approval status.
	// The agent system must call an approve endpoint before execution proceeds.
	RequireApproval bool `json:"require_approval,omitempty"`

	// Blocked prevents any execution in this mode.
	Blocked bool `json:"blocked,omitempty"`
}

// DefaultExecPolicy returns a sensible default policy.
func DefaultExecPolicy() ExecPolicy {
	return ExecPolicy{
		ModeRestrictions: map[ExecMode]ModeExecPolicy{
			ModeExplore: {
				ReadOnly: true,
				// All commands can run — filesystem is read-only (enforced by supervisor).
			},
			ModeExecute: {
				// Full access.
			},
			ModeAsk: {
				RequireApproval: true,
			},
		},
	}
}

// ReadOnlyCommands is a set of commands considered safe for read-only modes.
var ReadOnlyCommands = map[string]bool{
	"cat": true, "ls": true, "find": true, "grep": true, "rg": true,
	"ripgrep": true, "head": true, "tail": true, "less": true, "more": true,
	"wc": true, "file": true, "stat": true, "du": true, "df": true,
	"tree": true, "which": true, "whoami": true, "env": true, "printenv": true,
	"echo": true, "date": true, "uname": true, "hostname": true, "pwd": true,
	"id": true, "test": true, "true": true, "false": true, "diff": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "awk": true, "sed": true,
	"jq": true, "yq": true, "xxd": true, "hexdump": true, "md5sum": true, "sha256sum": true,
}

// ValidateCommand checks if a command is allowed by this policy in the given mode.
// Returns nil if allowed, or an error describing why it's blocked.
func (p *ExecPolicy) ValidateCommand(cmd Command, mode ExecMode) error {
	binary := extractBinary(cmd.Command)

	// 1. Check if mode blocks all execution.
	if modePolicy, ok := p.ModeRestrictions[mode]; ok {
		if modePolicy.Blocked {
			return fmt.Errorf("execution blocked in %s mode", mode)
		}
	}

	// 2. Check blocked commands (always enforced).
	if matchesAny(binary, p.BlockedCommands) {
		return fmt.Errorf("command %q is blocked by policy", binary)
	}

	// 3. Check mode-specific restrictions.
	// Note: ReadOnly is enforced at the filesystem level by the in-VM supervisor
	// (Landlock, mount flags), not by blocking commands. Any command can run in
	// explore mode — it just can't write to the filesystem.
	if modePolicy, ok := p.ModeRestrictions[mode]; ok {
		// 3a. Check mode-specific allowed commands.
		if len(modePolicy.AllowedCommands) > 0 {
			if !matchesAny(binary, modePolicy.AllowedCommands) {
				return fmt.Errorf("command %q not allowed in %s mode", binary, mode)
			}
		}
	}

	// 5. Check session-level allowed commands.
	if len(p.AllowedCommands) > 0 {
		if !matchesAny(binary, p.AllowedCommands) {
			return fmt.Errorf("command %q not in allowed list", binary)
		}
	}

	return nil
}

// EffectiveFilesystemPolicy returns the filesystem policy.
// If the ExecPolicy has an explicit FilesystemPolicy, that takes precedence.
// Otherwise, the default policy is used.
func (p *ExecPolicy) EffectiveFilesystemPolicy() FilesystemPolicy {
	if p.FilesystemPolicy != nil {
		return *p.FilesystemPolicy
	}
	return DefaultFilesystemPolicy()
}

// EffectiveNetworkPolicy returns the network policy for the current mode.
// If the ExecPolicy has an explicit NetworkPolicy, that takes precedence.
// Otherwise, the mode default is used.
func (p *ExecPolicy) EffectiveNetworkPolicy(mode ExecMode) NetworkPolicy {
	if p.NetworkPolicy != nil {
		return *p.NetworkPolicy
	}
	if np, ok := ModeNetworkPolicies[mode]; ok {
		return np
	}
	return DefaultNetworkPolicy()
}

// RequiresApproval checks if execution in the given mode requires approval.
func (p *ExecPolicy) RequiresApproval(mode ExecMode) bool {
	if modePolicy, ok := p.ModeRestrictions[mode]; ok {
		return modePolicy.RequireApproval
	}
	return false
}

func extractBinary(command string) string {
	// Handle paths: /usr/bin/python3 -> python3
	binary := filepath.Base(command)
	// Handle "sh -c ..." style — the binary is "sh"
	return binary
}

func matchesAny(binary string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == binary {
			return true
		}
		// Support glob patterns.
		if matched, _ := filepath.Match(pattern, binary); matched {
			return true
		}
	}
	return false
}

func isReadOnlyCommand(binary string) bool {
	return ReadOnlyCommands[binary]
}
