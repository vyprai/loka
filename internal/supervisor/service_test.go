package supervisor

import (
	"context"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- RingBuffer Tests ---

func TestRingBufferWrite(t *testing.T) {
	rb := NewRingBuffer(100)
	rb.WriteLine("line1")
	rb.WriteLine("line2")
	rb.WriteLine("line3")

	lines := rb.Lines(0)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestRingBufferOverflow(t *testing.T) {
	cap := 10
	rb := NewRingBuffer(cap)
	for i := 0; i < 25; i++ {
		rb.WriteLine("line")
	}

	lines := rb.Lines(0)
	if len(lines) != cap {
		t.Fatalf("expected %d lines after overflow, got %d", cap, len(lines))
	}
}

func TestRingBufferLinesLimit(t *testing.T) {
	rb := NewRingBuffer(1000)
	for i := 0; i < 100; i++ {
		rb.WriteLine("line")
	}

	lines := rb.Lines(10)
	if len(lines) != 10 {
		t.Fatalf("expected 10 lines, got %d", len(lines))
	}
}

func TestRingBufferEmpty(t *testing.T) {
	rb := NewRingBuffer(100)
	lines := rb.Lines(0)
	if lines != nil {
		t.Fatalf("expected nil from empty buffer, got %v", lines)
	}
	lines2 := rb.Lines(10)
	if lines2 != nil {
		t.Fatalf("expected nil from empty buffer with limit, got %v", lines2)
	}
}

func TestRingBufferConcurrent(t *testing.T) {
	rb := NewRingBuffer(100)
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rb.WriteLine("concurrent-line")
			}
		}()
	}

	// Concurrent readers.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = rb.Lines(10)
			}
		}()
	}

	wg.Wait()
	// If we get here without a panic, the test passes.
}

func TestRingBufferIOWriter(t *testing.T) {
	rb := NewRingBuffer(100)
	// Test the io.Writer interface which splits on newlines.
	data := "hello\nworld\nfoo\n"
	n, err := rb.Write([]byte(data))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes written, got %d", len(data), n)
	}
	lines := rb.Lines(0)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "hello" || lines[1] != "world" || lines[2] != "foo" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestRingBufferPartialWrite(t *testing.T) {
	rb := NewRingBuffer(100)
	// Write without trailing newline — should be held as partial.
	rb.Write([]byte("partial"))
	lines := rb.Lines(0)
	if lines != nil {
		t.Fatalf("expected nil (partial not flushed), got %v", lines)
	}
	// Complete the line.
	rb.Write([]byte(" complete\n"))
	lines = rb.Lines(0)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0] != "partial complete" {
		t.Errorf("expected 'partial complete', got %q", lines[0])
	}
}

// --- ServiceProcess Tests ---

func TestServiceProcessStart(t *testing.T) {
	sp := NewServiceProcess("sleep", []string{"60"}, map[string]string{}, "", "never")
	if err := sp.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sp.Stop(syscall.SIGTERM, 5*time.Second)

	status := sp.Status()
	if !status.Running {
		t.Error("expected process to be running")
	}
	if status.PID == 0 {
		t.Error("expected non-zero PID")
	}
}

func TestServiceProcessStop(t *testing.T) {
	sp := NewServiceProcess("sleep", []string{"60"}, map[string]string{}, "", "never")
	if err := sp.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if err := sp.Stop(syscall.SIGTERM, 5*time.Second); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	// Wait for the stopped channel to close.
	select {
	case <-sp.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process to stop")
	}

	status := sp.Status()
	if status.Running {
		t.Error("expected process to not be running after stop")
	}
}

func TestServiceProcessStatus(t *testing.T) {
	sp := NewServiceProcess("sleep", []string{"60"}, map[string]string{}, "", "never")
	if err := sp.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sp.Stop(syscall.SIGTERM, 5*time.Second)

	// Give a moment for the process to be fully started.
	time.Sleep(50 * time.Millisecond)

	status := sp.Status()
	if !status.Running {
		t.Error("expected running = true")
	}
	if status.PID <= 0 {
		t.Errorf("expected positive PID, got %d", status.PID)
	}
	if status.UptimeSeconds <= 0 {
		t.Errorf("expected positive uptime, got %f", status.UptimeSeconds)
	}
	if status.StartedAt.IsZero() {
		t.Error("expected non-zero StartedAt")
	}
}

func TestServiceProcessRestartOnFailure(t *testing.T) {
	// Process exits with code 1 — should restart with "on-failure" policy.
	sp := NewServiceProcess("sh", []string{"-c", "exit 1"}, map[string]string{}, "", "on-failure")
	if err := sp.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sp.Stop(syscall.SIGTERM, 5*time.Second)

	// Wait enough time for at least one restart (1s backoff + process run time).
	time.Sleep(2500 * time.Millisecond)

	status := sp.Status()
	if status.Restarts < 1 {
		t.Errorf("expected at least 1 restart, got %d", status.Restarts)
	}
}

func TestServiceProcessRestartNever(t *testing.T) {
	// Process exits with code 1 — should NOT restart with "never" policy.
	sp := NewServiceProcess("sh", []string{"-c", "exit 1"}, map[string]string{}, "", "never")
	if err := sp.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for the process loop to exit.
	select {
	case <-sp.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process to exit")
	}

	status := sp.Status()
	if status.Restarts != 0 {
		t.Errorf("expected 0 restarts with never policy, got %d", status.Restarts)
	}
	if status.Running {
		t.Error("expected process to not be running")
	}
	if status.ExitCode != 1 {
		t.Errorf("expected exit code 1, got %d", status.ExitCode)
	}
}

func TestServiceProcessStdout(t *testing.T) {
	sp := NewServiceProcess("sh", []string{"-c", "echo hello"}, map[string]string{}, "", "never")
	if err := sp.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for the process to finish.
	select {
	case <-sp.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process to finish")
	}

	lines := sp.stdout.Lines(0)
	if len(lines) == 0 {
		t.Fatal("expected stdout output, got none")
	}

	found := false
	for _, line := range lines {
		if strings.Contains(line, "hello") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'hello' in stdout, got %v", lines)
	}
}
