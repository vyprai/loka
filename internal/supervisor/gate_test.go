package supervisor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
)

func TestGate_ApproveResumes(t *testing.T) {
	var notified bool
	gate := NewApprovalGate(5*time.Second, func(pa *PendingApproval) {
		notified = true
	})

	cmd := loka.Command{ID: "cmd-1", Command: "wget"}
	ctx := context.Background()

	var wg sync.WaitGroup
	var result *ApprovalDecision
	var suspendErr error

	// Suspend in background.
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, suspendErr = gate.Suspend(ctx, "session-1", cmd, "not whitelisted")
	}()

	// Wait for the command to be suspended.
	time.Sleep(50 * time.Millisecond)

	if !notified {
		t.Error("onPending callback should have been called")
	}
	if gate.PendingCount() != 1 {
		t.Errorf("pending count = %d, want 1", gate.PendingCount())
	}

	// Approve it.
	if err := gate.Approve("cmd-1", false); err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	if suspendErr != nil {
		t.Errorf("suspend should succeed after approve, got: %v", suspendErr)
	}
	if result == nil || !result.Approved {
		t.Error("result should be approved")
	}
	if gate.PendingCount() != 0 {
		t.Errorf("pending count after approve = %d, want 0", gate.PendingCount())
	}
}

func TestGate_DenyReturnsError(t *testing.T) {
	gate := NewApprovalGate(5*time.Second, nil)
	cmd := loka.Command{ID: "cmd-2", Command: "rm"}
	ctx := context.Background()

	var wg sync.WaitGroup
	var suspendErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, suspendErr = gate.Suspend(ctx, "session-1", cmd, "blocked")
	}()

	time.Sleep(50 * time.Millisecond)

	gate.Deny("cmd-2", "too dangerous")
	wg.Wait()

	if suspendErr == nil {
		t.Error("deny should return an error")
	}
	if suspendErr.Error() != `command "rm" denied: too dangerous` {
		t.Errorf("unexpected error: %v", suspendErr)
	}
}

func TestGate_TimeoutReturnsError(t *testing.T) {
	gate := NewApprovalGate(100*time.Millisecond, nil) // Short timeout.
	cmd := loka.Command{ID: "cmd-3", Command: "slow"}
	ctx := context.Background()

	_, err := gate.Suspend(ctx, "session-1", cmd, "waiting")
	if err == nil {
		t.Error("timeout should return an error")
	}
	if gate.PendingCount() != 0 {
		t.Error("timed out command should be cleaned up")
	}
}

func TestGate_ContextCancelReturnsError(t *testing.T) {
	gate := NewApprovalGate(5*time.Second, nil)
	cmd := loka.Command{ID: "cmd-4", Command: "canceled"}
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var suspendErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, suspendErr = gate.Suspend(ctx, "session-1", cmd, "waiting")
	}()

	time.Sleep(50 * time.Millisecond)
	cancel() // Cancel the context.
	wg.Wait()

	if suspendErr == nil {
		t.Error("context cancel should return an error")
	}
}

func TestGate_ApproveWithWhitelist(t *testing.T) {
	gate := NewApprovalGate(5*time.Second, nil)
	cmd := loka.Command{ID: "cmd-5", Command: "custom-tool"}
	ctx := context.Background()

	var result *ApprovalDecision
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		result, _ = gate.Suspend(ctx, "session-1", cmd, "not whitelisted")
	}()

	time.Sleep(50 * time.Millisecond)
	gate.Approve("cmd-5", true) // Approve WITH whitelist.
	wg.Wait()

	if result == nil || !result.AddToWhitelist {
		t.Error("result should have AddToWhitelist=true")
	}
}

func TestGate_ListPending(t *testing.T) {
	gate := NewApprovalGate(5*time.Second, nil)
	ctx := context.Background()

	// Suspend 3 commands.
	for i := 0; i < 3; i++ {
		cmd := loka.Command{ID: fmt.Sprintf("cmd-%d", i), Command: fmt.Sprintf("tool-%d", i)}
		go gate.Suspend(ctx, "session-1", cmd, "waiting")
	}

	time.Sleep(50 * time.Millisecond)

	pending := gate.ListPending()
	if len(pending) != 3 {
		t.Errorf("pending = %d, want 3", len(pending))
	}

	// Approve all.
	for _, pa := range pending {
		gate.Approve(pa.ID, false)
	}

	time.Sleep(50 * time.Millisecond)
	if gate.PendingCount() != 0 {
		t.Errorf("all should be cleared, got %d", gate.PendingCount())
	}
}

func TestGate_ApproveNonexistent(t *testing.T) {
	gate := NewApprovalGate(5*time.Second, nil)
	err := gate.Approve("nonexistent", false)
	if err == nil {
		t.Error("approving nonexistent should error")
	}
}

func TestGate_ApproveRemovesFromPending(t *testing.T) {
	gate := NewApprovalGate(5*time.Second, nil)
	cmd := loka.Command{ID: "cmd-cleanup-1", Command: "test"}
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		gate.Suspend(ctx, "session-1", cmd, "waiting")
	}()

	time.Sleep(50 * time.Millisecond)

	// Approve should immediately remove from pending.
	if err := gate.Approve("cmd-cleanup-1", false); err != nil {
		t.Fatal(err)
	}

	// Double approve should fail — already removed.
	err := gate.Approve("cmd-cleanup-1", false)
	if err == nil {
		t.Error("second Approve should fail because entry was already removed")
	}

	wg.Wait()
	if gate.PendingCount() != 0 {
		t.Errorf("pending count = %d, want 0", gate.PendingCount())
	}
}

func TestGate_DenyRemovesFromPending(t *testing.T) {
	gate := NewApprovalGate(5*time.Second, nil)
	cmd := loka.Command{ID: "cmd-cleanup-2", Command: "test"}
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		gate.Suspend(ctx, "session-1", cmd, "waiting")
	}()

	time.Sleep(50 * time.Millisecond)

	// Deny should immediately remove from pending.
	if err := gate.Deny("cmd-cleanup-2", "nope"); err != nil {
		t.Fatal(err)
	}

	// Double deny should fail — already removed.
	err := gate.Deny("cmd-cleanup-2", "nope again")
	if err == nil {
		t.Error("second Deny should fail because entry was already removed")
	}

	wg.Wait()
	if gate.PendingCount() != 0 {
		t.Errorf("pending count = %d, want 0", gate.PendingCount())
	}
}
