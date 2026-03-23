package ha

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLocalCoordinator_LockUnlock(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	unlock, err := c.Lock(context.Background(), "test-key", 5*time.Second)
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}

	// Unlock should not panic.
	unlock()
}

func TestLocalCoordinator_LockContention(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	// Acquire the first lock.
	unlock1, err := c.Lock(context.Background(), "contended", 5*time.Second)
	if err != nil {
		t.Fatalf("first Lock failed: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		// This should block until unlock1 is called.
		unlock2, err := c.Lock(context.Background(), "contended", 5*time.Second)
		if err != nil {
			t.Errorf("second Lock failed: %v", err)
			return
		}
		close(acquired)
		unlock2()
	}()

	// Give goroutine time to start and block.
	time.Sleep(50 * time.Millisecond)

	select {
	case <-acquired:
		t.Fatal("second lock should not have been acquired before first unlock")
	default:
		// Expected: still blocked.
	}

	// Release the first lock; the second should proceed.
	unlock1()

	select {
	case <-acquired:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("second lock was not acquired after first unlock")
	}
}

func TestLocalCoordinator_LockDifferentKeys(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	unlock1, err := c.Lock(context.Background(), "key-a", 5*time.Second)
	if err != nil {
		t.Fatalf("Lock key-a failed: %v", err)
	}
	defer unlock1()

	// A different key should not be blocked.
	unlock2, err := c.Lock(context.Background(), "key-b", 5*time.Second)
	if err != nil {
		t.Fatalf("Lock key-b failed: %v", err)
	}
	defer unlock2()
}

func TestLocalCoordinator_PublishSubscribe(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := c.Subscribe(ctx, "events")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	payload := []byte("hello world")
	if err := c.Publish(context.Background(), "events", payload); err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	select {
	case msg := <-ch:
		if string(msg) != "hello world" {
			t.Fatalf("expected 'hello world', got %q", string(msg))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for published message")
	}
}

func TestLocalCoordinator_PublishNoSubscribers(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	// Publishing to a topic with no subscribers should not error.
	err := c.Publish(context.Background(), "no-one-listening", []byte("data"))
	if err != nil {
		t.Fatalf("Publish with no subscribers should not fail: %v", err)
	}
}

func TestLocalCoordinator_SubscribeMultiple(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1, _ := c.Subscribe(ctx, "multi")
	ch2, _ := c.Subscribe(ctx, "multi")

	_ = c.Publish(context.Background(), "multi", []byte("msg"))

	for i, ch := range []<-chan []byte{ch1, ch2} {
		select {
		case msg := <-ch:
			if string(msg) != "msg" {
				t.Errorf("subscriber %d: expected 'msg', got %q", i, string(msg))
			}
		case <-time.After(2 * time.Second):
			t.Errorf("subscriber %d: timed out", i)
		}
	}
}

func TestLocalCoordinator_SubscribeContextCancel(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Subscribe(ctx, "cancel-test")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	// Cancel the context; the channel should be closed.
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			// Might receive a zero value before close; drain and check again.
		}
		// Channel closed -- expected.
	case <-time.After(2 * time.Second):
		t.Fatal("channel was not closed after context cancellation")
	}
}

func TestLocalCoordinator_ElectLeader(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	called := false
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		err := c.ElectLeader(ctx, "scheduler", func(ctx context.Context) {
			called = true
			// Verify IsLeader returns true while we hold leadership.
			if !c.IsLeader("scheduler") {
				t.Error("expected IsLeader to return true during leaderFunc")
			}
			cancel()
		})
		if err != nil {
			t.Errorf("ElectLeader failed: %v", err)
		}
	}()

	wg.Wait()

	if !called {
		t.Fatal("leaderFunc was not called")
	}

	// After ElectLeader returns, leadership should be relinquished.
	if c.IsLeader("scheduler") {
		t.Error("expected IsLeader to return false after ElectLeader returns")
	}
}

func TestLocalCoordinator_ElectLeaderContextCancel(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = c.ElectLeader(ctx, "timed", func(ctx context.Context) {
			// Block until context is cancelled.
			<-ctx.Done()
		})
		close(done)
	}()

	select {
	case <-done:
		// ElectLeader returned after context cancellation -- success.
	case <-time.After(5 * time.Second):
		t.Fatal("ElectLeader did not return after context cancellation")
	}
}

func TestLocalCoordinator_IsLeaderDefault(t *testing.T) {
	c := NewLocalCoordinator()
	defer c.Close()

	if c.IsLeader("anything") {
		t.Error("expected IsLeader to return false for unknown election")
	}
}

func TestLocalCoordinator_Close(t *testing.T) {
	c := NewLocalCoordinator()
	err := c.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
