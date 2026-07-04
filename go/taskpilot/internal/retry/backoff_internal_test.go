package retry

import (
	"testing"
	"time"
)

// Shared backoff bounds for the Fibonacci backoff tests.
const (
	testInitialBackoff = time.Second
	testMaxBackoff     = 10 * time.Second
)

func TestFibonacciBackoff(t *testing.T) {
	initial := testInitialBackoff
	maximum := testMaxBackoff
	want := []time.Duration{
		time.Second,
		time.Second,
		2 * time.Second,
		3 * time.Second,
		5 * time.Second,
		8 * time.Second,
		10 * time.Second, // capped at maxBackoff
	}
	for i, expected := range want {
		if got := fibonacciBackoff(i+1, initial, maximum); got != expected {
			t.Fatalf("attempt %d backoff = %s, want %s", i+1, got, expected)
		}
	}
}

func TestFibonacciBackoffWithJitterHalfBounds(t *testing.T) {
	initial := testInitialBackoff
	maximum := testMaxBackoff
	for attempt := 1; attempt <= 8; attempt++ {
		base := fibonacciBackoff(attempt, initial, maximum)
		for range 20 {
			got, err := fibonacciBackoffWithJitter(attempt, initial, maximum, 0.5)
			if err != nil {
				t.Fatalf("attempt %d jitter draw failed: %v", attempt, err)
			}
			if got < base/2 {
				t.Fatalf("attempt %d jittered = %s, below min %s", attempt, got, base/2)
			}
			if got > base {
				t.Fatalf("attempt %d jittered = %s, above base %s", attempt, got, base)
			}
		}
	}
}

func TestFibonacciBackoffWithJitterZeroIsNoJitter(t *testing.T) {
	initial := testInitialBackoff
	maximum := testMaxBackoff
	for attempt := 1; attempt <= 8; attempt++ {
		base := fibonacciBackoff(attempt, initial, maximum)
		for range 10 {
			got, err := fibonacciBackoffWithJitter(attempt, initial, maximum, 0)
			if err != nil {
				t.Fatalf("attempt %d jitter draw failed: %v", attempt, err)
			}
			if got != base {
				t.Fatalf("attempt %d jittered = %s, want exact base %s", attempt, got, base)
			}
		}
	}
}

func TestFibonacciBackoffWithJitterFullBounds(t *testing.T) {
	initial := testInitialBackoff
	maximum := testMaxBackoff
	for attempt := 1; attempt <= 8; attempt++ {
		base := fibonacciBackoff(attempt, initial, maximum)
		for range 20 {
			got, err := fibonacciBackoffWithJitter(attempt, initial, maximum, 1)
			if err != nil {
				t.Fatalf("attempt %d jitter draw failed: %v", attempt, err)
			}
			if got < 0 {
				t.Fatalf("attempt %d jittered = %s, below 0", attempt, got)
			}
			if got > base {
				t.Fatalf("attempt %d jittered = %s, above base %s", attempt, got, base)
			}
		}
	}
}
