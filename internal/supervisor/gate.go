package supervisor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/loka"
)

// ApprovalGate manages suspended commands waiting for approval.
// When the proxy returns needs_approval, the executor suspends the command
// here instead of failing. The gate holds the command until:
//   - Approved: the command proceeds to execution
//   - Denied: the command gets an error result
//   - Timeout: the command is auto-denied after a configurable duration
//   - Context canceled: the command is aborted
type ApprovalGate struct {
	mu       sync.Mutex
	pending  map[string]*PendingApproval
	timeout  time.Duration
	onPending func(pa *PendingApproval) // Callback when a command is suspended.
}

// PendingApproval represents a command suspended at the gate.
type PendingApproval struct {
	ID        string           // Exec ID (or command ID for parallel).
	SessionID string
	Command   loka.Command
	Reason    string           // Why approval is needed.
	Status    ApprovalStatus
	Created   time.Time

	// result channel — the executor blocks on this.
	resultCh chan ApprovalDecision
}

// ApprovalStatus tracks the state of a pending approval.
type ApprovalStatus string

const (
	ApprovalStatusWaiting  ApprovalStatus = "waiting"
	ApprovalStatusApproved ApprovalStatus = "approved"
	ApprovalStatusDenied   ApprovalStatus = "denied"
	ApprovalStatusTimeout  ApprovalStatus = "timeout"
)

// ApprovalDecision is sent through the channel to resume or abort a suspended command.
type ApprovalDecision struct {
	Approved       bool
	AddToWhitelist bool   // If true, permanently whitelist this command.
	Reason         string // Reason for denial (empty if approved).
}

// NewApprovalGate creates a new gate with the given timeout.
func NewApprovalGate(timeout time.Duration, onPending func(*PendingApproval)) *ApprovalGate {
	if timeout == 0 {
		timeout = 5 * time.Minute // Default: 5 minute timeout for approvals.
	}
	return &ApprovalGate{
		pending:   make(map[string]*PendingApproval),
		timeout:   timeout,
		onPending: onPending,
	}
}

const maxApprovalTimeout = 10 * time.Minute

// Suspend parks a command at the gate and blocks until a decision is made.
// Returns nil if approved (caller should proceed to execute).
// Returns an error if denied or timed out.
// A hard maximum timeout of 10 minutes is enforced regardless of the
// configured timeout or context, to prevent goroutine leaks.
func (g *ApprovalGate) Suspend(ctx context.Context, sessionID string, cmd loka.Command, reason string) (*ApprovalDecision, error) {
	pa := &PendingApproval{
		ID:        cmd.ID,
		SessionID: sessionID,
		Command:   cmd,
		Reason:    reason,
		Status:    ApprovalStatusWaiting,
		Created:   time.Now(),
		resultCh:  make(chan ApprovalDecision, 1),
	}

	g.mu.Lock()
	g.pending[cmd.ID] = pa
	g.mu.Unlock()

	// Notify the callback that a command is waiting.
	if g.onPending != nil {
		g.onPending(pa)
	}

	// Use the configured timeout, but cap at the hard maximum.
	timeout := g.timeout
	if timeout > maxApprovalTimeout || timeout <= 0 {
		timeout = maxApprovalTimeout
	}

	// Block until decision, timeout, or context cancellation.
	select {
	case decision := <-pa.resultCh:
		// Approve/Deny already removed from pending map; clean up just in case.
		g.mu.Lock()
		if decision.Approved {
			pa.Status = ApprovalStatusApproved
		} else {
			pa.Status = ApprovalStatusDenied
		}
		delete(g.pending, cmd.ID)
		g.mu.Unlock()

		if !decision.Approved {
			reason := decision.Reason
			if reason == "" {
				reason = "denied by operator"
			}
			return &decision, fmt.Errorf("command %q denied: %s", cmd.Command, reason)
		}
		return &decision, nil

	case <-time.After(timeout):
		g.mu.Lock()
		pa.Status = ApprovalStatusTimeout
		delete(g.pending, cmd.ID)
		g.mu.Unlock()
		return nil, fmt.Errorf("command %q approval timed out after %s", cmd.Command, timeout)

	case <-ctx.Done():
		g.mu.Lock()
		delete(g.pending, cmd.ID)
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Approve resumes a suspended command.
func (g *ApprovalGate) Approve(cmdID string, addToWhitelist bool) error {
	g.mu.Lock()
	pa, ok := g.pending[cmdID]
	if ok {
		delete(g.pending, cmdID) // Clean up immediately to prevent double approve/deny.
	}
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending approval for command %s", cmdID)
	}

	pa.resultCh <- ApprovalDecision{Approved: true, AddToWhitelist: addToWhitelist}
	return nil
}

// Deny rejects a suspended command with a reason.
func (g *ApprovalGate) Deny(cmdID string, reason string) error {
	g.mu.Lock()
	pa, ok := g.pending[cmdID]
	if ok {
		delete(g.pending, cmdID) // Clean up immediately to prevent double approve/deny.
	}
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending approval for command %s", cmdID)
	}

	pa.resultCh <- ApprovalDecision{Approved: false, Reason: reason}
	return nil
}

// ListPending returns all commands waiting for approval.
func (g *ApprovalGate) ListPending() []*PendingApproval {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make([]*PendingApproval, 0, len(g.pending))
	for _, pa := range g.pending {
		result = append(result, pa)
	}
	return result
}

// PendingCount returns the number of commands waiting.
func (g *ApprovalGate) PendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}
