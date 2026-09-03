package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestPacing verifies that requests are issued no faster than the configured rate.
// Uses 60 req/min (1 per second) so tests run in reasonable time.
func TestPacing(t *testing.T) {
	l := New(60) // 60 req/min = 1 req/s = 1000ms interval

	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(ctx); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	elapsed := time.Since(start)

	// 3 requests at 1/s: first is free, then two 1s gaps = ~2s total.
	// Allow generous tolerance for CI timing jitter.
	if elapsed < 1800*time.Millisecond {
		t.Errorf("too fast: elapsed=%v, want >= 1.8s", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("too slow: elapsed=%v, want < 4s", elapsed)
	}
}

// TestConcurrentSerialization verifies that concurrent Wait calls are serialized
// and produce the expected cumulative elapsed time.
func TestConcurrentSerialization(t *testing.T) {
	l := New(60) // 1 req/s
	ctx := context.Background()
	n := 4

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(ctx); err != nil {
				t.Errorf("Wait: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// n concurrent callers at 1/s: first is free, then 3 gaps = ~3s.
	if elapsed < 2500*time.Millisecond {
		t.Errorf("too fast: elapsed=%v, want >= 2.5s", elapsed)
	}
	if elapsed > 6*time.Second {
		t.Errorf("too slow: elapsed=%v, want < 6s", elapsed)
	}
}

// TestContextCancellation verifies that Wait returns ctx.Err() when cancelled.
func TestContextCancellation(t *testing.T) {
	l := New(1) // 1 req/min = 60s interval - ensures next Wait would block
	ctx := context.Background()

	// Consume the first free slot.
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	// Cancel immediately; second Wait should unblock and return error.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	start := time.Now()
	err := l.Wait(cancelCtx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Wait took too long after cancel: %v", elapsed)
	}
}
