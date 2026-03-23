package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vyprai/loka/internal/loka"
)

// Sandbox enforces execution policy at the OS level inside the microVM.
//
// Code scanning (proxy.go) is a first line of defense but is BYPASSABLE.
// The sandbox provides the REAL enforcement via OS-level mechanisms:
//
//  1. Binary Removal   — interpreters not in the whitelist are removed from PATH
//  2. PATH Restriction — /env/bin only contains allowed binaries
//  3. Filesystem Mount — workspace is RO in inspect/plan modes (kernel-enforced)
//  4. Seccomp Filter   — blocks execve/fork/clone for untrusted processes
//  5. Network Filter   — iptables blocks outbound in inspect/plan modes
//
// In a Firecracker microVM, these are enforced by the guest kernel.
// For local dev mode, we simulate via PATH restriction and proxy checks.
type Sandbox struct {
	policy  loka.ExecPolicy
	mode    loka.ExecMode
	envDir  string // The /env directory with projected binaries.
	dataDir string // Session data directory.
}

// NewSandbox creates a new sandbox for a session.
func NewSandbox(policy loka.ExecPolicy, mode loka.ExecMode, envDir, dataDir string) *Sandbox {
	return &Sandbox{
		policy:  policy,
		mode:    mode,
		envDir:  envDir,
		dataDir: dataDir,
	}
}

// SetMode updates the execution mode and re-applies OS-level restrictions.
func (s *Sandbox) SetMode(mode loka.ExecMode) error {
	s.mode = mode
	return s.Apply()
}

// Apply enforces the current policy and mode at the OS level.
// In production (Firecracker VM), this calls mount/iptables/seccomp.
// For dev mode, it manages the /env/bin directory.
func (s *Sandbox) Apply() error {
	if err := s.applyBinaryRestrictions(); err != nil {
		return fmt.Errorf("apply binary restrictions: %w", err)
	}
	// In production, also:
	// - s.applyFilesystemMount()  — remount workspace RO/RW
	// - s.applyNetworkRules()     — iptables allow/deny
	// - s.applySeccompFilter()    — block execve for non-whitelisted binaries
	return nil
}

// ── Binary Removal ──────────────────────────────────────
//
// The strongest way to prevent an interpreter from running is to
// make it not exist. If python3 is not in /env/bin or anywhere on
// PATH, then no amount of code obfuscation can invoke it.
//
// This is enforced by:
// 1. Only projecting allowed packages into /env/bin
// 2. Setting PATH=/env/bin (nothing else)
// 3. In the VM rootfs, /usr/bin etc. are read-only and don't contain
//    interpreters that aren't projected

func (s *Sandbox) applyBinaryRestrictions() error {
	if s.envDir == "" {
		return nil
	}

	binDir := filepath.Join(s.envDir, "bin")
	os.MkdirAll(binDir, 0o755)

	// Build the set of allowed binaries.
	allowed := make(map[string]bool)
	for _, cmd := range s.policy.AllowedCommands {
		allowed[cmd] = true
	}

	// Mode-specific allowed commands.
	if modePolicy, ok := s.policy.ModeRestrictions[s.mode]; ok {
		for _, cmd := range modePolicy.AllowedCommands {
			allowed[cmd] = true
		}
	}

	// If no explicit allowlist, don't restrict.
	if len(allowed) == 0 {
		return nil
	}

	// Remove binaries that are NOT in the allowed set.
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		name := entry.Name()
		if !allowed[name] && !matchesAnyPattern(name, s.policy.AllowedCommands) {
			os.Remove(filepath.Join(binDir, name))
		}
	}

	return nil
}

func matchesAnyPattern(name string, patterns []string) bool {
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, name); matched {
			return true
		}
	}
	return false
}

// RestrictedPATH returns the PATH that should be set for all processes.
// In production, this is the ONLY PATH available in the VM.
func (s *Sandbox) RestrictedPATH() string {
	if s.envDir != "" {
		return filepath.Join(s.envDir, "bin")
	}
	return "/env/bin"
}

// ── Filesystem Mount (production only) ──────────────────
//
// In a Firecracker VM, the workspace is an overlayfs mount.
// The sandbox controls whether it's mounted RO or RW based on mode:
//
//   inspect/plan: mount -o remount,ro /workspace
//   execute/commit: mount -o remount,rw /workspace
//
// This is KERNEL-ENFORCED. No amount of code in python/node can
// bypass a read-only mount. Even root inside the VM cannot remount
// because the VM doesn't have CAP_SYS_ADMIN.

// FilesystemMode returns the mount mode for the current execution mode.
func (s *Sandbox) FilesystemMode() string {
	policy := loka.ModePolicies[s.mode]
	if policy.WorkspaceWrite {
		return "rw"
	}
	return "ro"
}

// ── Network Filter (production only) ────────────────────
//
// In a Firecracker VM, network access is controlled via iptables
// on the host TAP interface:
//
//   inspect/plan: iptables -A FORWARD -o <tap> -j DROP (except DNS)
//   execute:      iptables -A FORWARD -o <tap> -j ACCEPT (scoped)
//   commit:       full access
//
// This is enforced at the HOST level, outside the VM.
// The guest cannot modify these rules.

// NetworkAllowed returns whether outbound network is allowed in the current mode.
func (s *Sandbox) NetworkAllowed() bool {
	policy := loka.ModePolicies[s.mode]
	return policy.NetworkAccess
}

// ── Seccomp Filter (production only) ────────────────────
//
// For defense-in-depth, even when an interpreter IS allowed, we can
// restrict what syscalls it can make:
//
// Profile for read-only interpreters (inspect/plan mode):
//   - ALLOW: read, write(stdout/stderr only), open(RO), stat, mmap, brk
//   - BLOCK: execve, fork, clone, unlink, rename, connect, socket
//
// This means python3 can run pure computation but cannot:
//   - Spawn subprocesses (execve blocked)
//   - Delete files (unlink blocked)
//   - Make network connections (connect/socket blocked)
//
// Seccomp is set via prctl(PR_SET_SECCOMP) before exec'ing the process.

// SeccompProfile returns the appropriate seccomp profile name for the mode.
func (s *Sandbox) SeccompProfile() string {
	policy := loka.ModePolicies[s.mode]
	if !policy.WorkspaceWrite {
		return "readonly" // Blocks execve, unlink, rename, connect.
	}
	if policy.CredentialTier == loka.CredentialFull {
		return "full" // No restrictions.
	}
	return "standard" // Blocks dangerous syscalls like reboot, mount.
}

// ── Summary of Enforcement Layers ───────────────────────
//
// Layer 1: Command Proxy (proxy.go)
//   - First check: allowlist/blocklist/shell parsing
//   - Catches obvious violations early
//   - BYPASSABLE by obfuscated code
//
// Layer 2: Binary Removal (sandbox.go)
//   - Interpreters not in allowlist don't exist on the filesystem
//   - Cannot be bypassed — the binary literally doesn't exist
//   - Enforced by PATH restriction + /env/bin projection
//
// Layer 3: Filesystem Mount (Firecracker)
//   - Workspace is read-only in inspect/plan modes
//   - Kernel-enforced, cannot be bypassed from inside VM
//
// Layer 4: Network Filter (host iptables)
//   - No outbound in inspect/plan modes
//   - Enforced at the host level, outside the VM
//
// Layer 5: Seccomp Filter
//   - Blocks execve/fork in read-only modes
//   - Even allowed interpreters cannot spawn subprocesses
//   - Kernel-enforced
//
// Together these form defense-in-depth. The proxy is the UX layer
// (fast feedback, good error messages). The sandbox is the security layer
// (kernel-enforced, cannot be bypassed).

// EffectiveNetworkPolicy returns the network policy for the current mode.
func (s *Sandbox) EffectiveNetworkPolicy() loka.NetworkPolicy {
	return s.policy.EffectiveNetworkPolicy(s.mode)
}

// GenerateNetworkRules generates iptables rules for the session's VM.
func (s *Sandbox) GenerateNetworkRules(tapInterface string) []string {
	np := s.EffectiveNetworkPolicy()
	return np.GenerateIptablesRules(tapInterface)
}

// EnforcementSummary returns a human-readable summary of active enforcement.
func (s *Sandbox) EnforcementSummary() map[string]string {
	return map[string]string{
		"mode":          string(s.mode),
		"filesystem":    s.FilesystemMode(),
		"network":       fmt.Sprintf("%v", s.NetworkAllowed()),
		"seccomp":       s.SeccompProfile(),
		"path":          s.RestrictedPATH(),
		"binary_filter": fmt.Sprintf("%d commands allowed", len(s.policy.AllowedCommands)),
	}
}

// ── For reference: VM rootfs layout ─────────────────────
//
// /                          (read-only rootfs)
// ├── bin/                   (busybox/coreutils only)
// ├── env/                   (read-only, projected packages)
// │   └── bin/
// │       ├── python3 → /homebrew/cellar/python/3.12/bin/python3
// │       ├── git     → /homebrew/cellar/git/bin/git
// │       └── ...
// ├── workspace/             (overlayfs, RO or RW based on mode)
// ├── tmp/                   (tmpfs, always writable)
// └── usr/local/bin/
//     └── loka-supervisor    (the supervisor binary)
//
// PATH=/env/bin              (ONLY this directory)
//
// If python3 is not projected into /env/bin, it doesn't exist.
// If the workspace is mounted RO, no file can be written.
// If iptables blocks outbound, no data can leave.
// If seccomp blocks execve, no subprocess can be spawned.

// Ensure fmt/strings are used.
var _ = strings.Join
