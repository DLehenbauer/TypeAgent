package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestThrottleSubmitIsNonBlocking verifies dispatch returns immediately even
// when all concurrency slots are occupied: the caller gets a Future without
// waiting for a slot to free.
func TestThrottleSubmitIsNonBlocking(t *testing.T) {
	tr := newThrottle(1)
	release := make(chan struct{})
	// Occupy the single slot with a blocked item.
	blocked := tr.dispatch(context.Background(), func(context.Context) (Result, error) {
		<-release
		return "first", nil
	})

	done := make(chan struct{})
	go func() {
		// This submission cannot acquire the slot yet, but dispatch must not
		// block the goroutine that calls it.
		tr.dispatch(context.Background(), func(context.Context) (Result, error) {
			return "second", nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch blocked while slot was occupied")
	}

	close(release)
	if _, err := blocked.Await(context.Background()); err != nil {
		t.Fatalf("blocked.Await: %v", err)
	}
}

// TestThrottleCapsConcurrency verifies at most N items run at once.
func TestThrottleCapsConcurrency(t *testing.T) {
	const limit = 3
	const total = 12
	tr := newThrottle(limit)

	var running, maxSeen int32
	futures := make([]Future, total)
	for i := 0; i < total; i++ {
		futures[i] = tr.dispatch(context.Background(), func(context.Context) (Result, error) {
			cur := atomic.AddInt32(&running, 1)
			for {
				m := atomic.LoadInt32(&maxSeen)
				if cur <= m || atomic.CompareAndSwapInt32(&maxSeen, m, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&running, -1)
			return nil, nil
		})
	}
	for _, f := range futures {
		if _, err := f.Await(context.Background()); err != nil {
			t.Fatalf("Await: %v", err)
		}
	}
	if maxSeen > limit {
		t.Fatalf("observed %d concurrent items, want <= %d", maxSeen, limit)
	}
}

// TestThrottleUnboundedRunsAllConcurrently verifies a non-positive limit does
// not gate concurrency.
func TestThrottleUnboundedRunsAllConcurrently(t *testing.T) {
	const total = 8
	tr := newThrottle(0)

	var ready sync.WaitGroup
	ready.Add(total)
	release := make(chan struct{})
	futures := make([]Future, total)
	for i := 0; i < total; i++ {
		futures[i] = tr.dispatch(context.Background(), func(context.Context) (Result, error) {
			ready.Done()
			<-release
			return nil, nil
		})
	}

	done := make(chan struct{})
	go func() { ready.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unbounded throttle did not run all items concurrently")
	}
	close(release)
	for _, f := range futures {
		if _, err := f.Await(context.Background()); err != nil {
			t.Fatalf("Await: %v", err)
		}
	}
}

// TestThrottleCancelsQueuedItemWithoutRunning verifies that an item cancelled
// while still waiting for a slot returns ctx.Err() and never executes fn.
func TestThrottleCancelsQueuedItemWithoutRunning(t *testing.T) {
	tr := newThrottle(1)
	started := make(chan struct{})
	release := make(chan struct{})
	blocking := tr.dispatch(context.Background(), func(context.Context) (Result, error) {
		close(started)
		<-release
		return nil, nil
	})
	// Ensure the blocking item has acquired the only slot before we queue the
	// next one, so the queued item is guaranteed to wait on Acquire.
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	var ran int32
	queued := tr.dispatch(ctx, func(context.Context) (Result, error) {
		atomic.StoreInt32(&ran, 1)
		return nil, nil
	})
	// Give the queued goroutine time to block on Acquire, then cancel it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	if _, err := queued.Await(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued.Await err = %v, want context.Canceled", err)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatal("queued item ran despite being cancelled before acquiring a slot")
	}

	close(release)
	if _, err := blocking.Await(context.Background()); err != nil {
		t.Fatalf("blocking.Await: %v", err)
	}
}
