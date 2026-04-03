package api

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestLimiter() *authRateLimiter {
	return &authRateLimiter{failures: make(map[string][]time.Time)}
}

func TestRateLimiter_BlocksAfterMaxFailures(t *testing.T) {
	rl := newTestLimiter()
	ip := "10.0.0.1"

	for i := 0; i < rateLimitMax; i++ {
		rl.recordFailure(ip)
	}

	if !rl.isBlocked(ip) {
		t.Fatal("expected IP to be blocked after max failures")
	}
}

func TestRateLimiter_NotBlockedBelowMax(t *testing.T) {
	rl := newTestLimiter()
	ip := "10.0.0.2"

	for i := 0; i < rateLimitMax-1; i++ {
		rl.recordFailure(ip)
	}

	if rl.isBlocked(ip) {
		t.Fatal("should not be blocked below max failures")
	}
}

func TestRateLimiter_UnblocksAfterDuration(t *testing.T) {
	rl := newTestLimiter()
	ip := "10.0.0.3"

	// Record old failures (beyond block duration).
	past := time.Now().Add(-rateLimitBlockDuration - time.Second)
	rl.failures[ip] = make([]time.Time, rateLimitMax)
	for i := range rl.failures[ip] {
		rl.failures[ip][i] = past
	}

	if rl.isBlocked(ip) {
		t.Fatal("should not be blocked after block duration expires")
	}
}

func TestRateLimiter_PrunesOldEntries(t *testing.T) {
	rl := newTestLimiter()
	ip := "10.0.0.4"

	// Record some old failures outside the window.
	past := time.Now().Add(-rateLimitWindow - time.Second)
	for i := 0; i < 5; i++ {
		rl.failures[ip] = append(rl.failures[ip], past)
	}

	// Check isBlocked prunes them.
	rl.isBlocked(ip)
	if len(rl.failures[ip]) != 0 {
		t.Errorf("expected old entries pruned, got %d", len(rl.failures[ip]))
	}
}

func TestRateLimiter_DifferentIPsIndependent(t *testing.T) {
	rl := newTestLimiter()

	for i := 0; i < rateLimitMax; i++ {
		rl.recordFailure("10.0.0.1")
	}

	if rl.isBlocked("10.0.0.2") {
		t.Fatal("different IP should not be blocked")
	}
}

func TestRateLimiter_ConcurrentAccess(t *testing.T) {
	rl := newTestLimiter()

	var wg sync.WaitGroup
	// 50 goroutines recording failures from different IPs.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip := "10.0.0.1" // Same IP — worst case.
			rl.recordFailure(ip)
			rl.isBlocked(ip)
		}(i)
	}
	wg.Wait()

	// No panic = success.
	if !rl.isBlocked("10.0.0.1") {
		t.Error("expected blocked after 50 failures")
	}
}

func TestRateLimiter_MapPruningOnLargeSize(t *testing.T) {
	rl := newTestLimiter()

	// Fill with many unique IPs.
	for i := 0; i < 10001; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
		rl.failures[ip] = []time.Time{time.Now().Add(-rateLimitBlockDuration - time.Second)}
	}

	// Recording a new failure should trigger pruning.
	rl.recordFailure("192.168.1.1")

	if len(rl.failures) >= 10001 {
		t.Errorf("expected pruning to reduce map size, got %d entries", len(rl.failures))
	}
}

func TestRateLimiter_RecordAndCheckAtomic(t *testing.T) {
	rl := newTestLimiter()
	ip := "10.0.0.99"

	var wg sync.WaitGroup
	// Concurrent record + check from same IP.
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rl.recordFailure(ip)
		}()
		go func() {
			defer wg.Done()
			rl.isBlocked(ip)
		}()
	}
	wg.Wait()

	// Should be blocked after 100 failures.
	if !rl.isBlocked(ip) {
		t.Error("expected blocked after 100 concurrent failures")
	}
}
