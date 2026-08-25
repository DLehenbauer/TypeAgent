// Package retry provides a reusable attempt loop with configurable policy,
// Fibonacci backoff, jitter, and pluggable classification hooks so each task
// can decide what counts as a retryable error and a retryable result.
package retry

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"time"
)

// Policy defines how many times and how quickly to retry a task execution.
type Policy struct {
	MaxAttempts    int           // Total number of attempts (1 = no retry)
	InitialBackoff time.Duration // Delay after the first failure
	MaxBackoff     time.Duration // Upper bound on backoff delay
	Jitter         float64       // Jitter factor in [0,1] where 0 = none and 1 = full
}

// DefaultPolicy returns a conservative no-retry policy used when a task does not
// specify one. It returns a fresh value on each call so callers can mutate the
// result without affecting anyone else.
func DefaultPolicy() Policy {
	return Policy{MaxAttempts: 1}
}

// Verdict classifies a completed (non-error) attempt's result.
type Verdict int

const (
	// Done accepts the result as a success; Run returns it.
	Done Verdict = iota
	// Retry schedules another attempt (e.g. polling until a condition holds).
	Retry
	// Fail rejects the result; Run returns a *ResultError.
	Fail
)

// Attempt carries per-attempt context to the classification hooks.
type Attempt struct {
	// Num is the 1-based attempt number.
	Num int
}

// ResultError wraps a body result that OnResult rejected (Fail) or that never
// satisfied OnResult before the attempt budget was exhausted. Surfacing it as
// the final error ensures the attempt is treated as a failure (and therefore is
// never cached) while still letting callers inspect the offending result.
type ResultError struct {
	Attempts  int
	Result    any
	Exhausted bool
	// Detail is an optional human-readable explanation of why the result was
	// rejected, produced by Options.DescribeResult. It lets a task surface the
	// real cause (e.g. a script's exit code and stderr) in the error message.
	Detail string
}

// Error reports whether the attempt budget was exhausted or the result was
// rejected, including the attempt count and appending Detail (when set) after a
// colon.
func (e *ResultError) Error() string {
	var base string
	if e.Exhausted {
		base = fmt.Sprintf("retry exhausted after %d attempt(s) without success", e.Attempts)
	} else {
		base = fmt.Sprintf("result rejected after %d attempt(s)", e.Attempts)
	}
	if e.Detail != "" {
		return base + ": " + e.Detail
	}
	return base
}

// newResultError builds a ResultError, filling Detail from opts.DescribeResult
// when provided so the rejection reason survives into the error message.
func newResultError(opts Options, attempt int, output any, exhausted bool) *ResultError {
	e := &ResultError{Attempts: attempt, Result: output, Exhausted: exhausted}
	if opts.DescribeResult != nil {
		e.Detail = opts.DescribeResult(output)
	}
	return e
}

// Options configure a single Run call. The zero value performs no retry: the
// body runs once and its error or result is returned as-is.
type Options struct {
	Policy Policy

	// OnError classifies an error returned by the body. Return true to schedule
	// another attempt. If nil, errors are never retried.
	OnError func(err error, at Attempt) bool

	// OnResult classifies a successful (non-error) body result as Done, Retry, or
	// Fail. If nil, every result is Done.
	OnResult func(result any, at Attempt) Verdict

	// DescribeResult, when set, produces a human-readable explanation of a
	// rejected result. It populates ResultError.Detail for a Fail verdict (or a
	// Retry verdict that exhausts the budget), so the cause is visible in the
	// final error message instead of a generic "result rejected".
	DescribeResult func(result any) string
}

// Run executes body up to Policy.MaxAttempts times, sleeping between attempts
// using Fibonacci backoff with jitter. ctx cancellation is honoured during sleep.
//
// On success it returns the accepted result. A retryable error that exhausts the
// budget is returned as-is. A Fail verdict, or a Retry verdict that exhausts the
// budget, is returned as a *ResultError so the result is never mistaken for a
// success.
func Run(ctx context.Context, opts Options, body func(context.Context) (any, error)) (any, error) {
	p := opts.Policy
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.MaxBackoff < p.InitialBackoff {
		p.MaxBackoff = p.InitialBackoff
	}
	if p.Jitter < 0 {
		p.Jitter = 0
	} else if p.Jitter > 1 {
		p.Jitter = 1
	}

	var lastErr error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		at := Attempt{Num: attempt}
		output, err := body(ctx)
		if err != nil {
			lastErr = err
			if attempt >= p.MaxAttempts || opts.OnError == nil || !opts.OnError(err, at) {
				return nil, lastErr
			}
			backoff, backoffErr := fibonacciBackoffWithJitter(attempt, p.InitialBackoff, p.MaxBackoff, p.Jitter)
			if backoffErr != nil {
				return nil, fmt.Errorf("retry aborted after attempt %d: %w (last error: %v)", attempt, backoffErr, lastErr)
			}
			if sleepErr := sleepContext(ctx, backoff); sleepErr != nil {
				return nil, fmt.Errorf("retry interrupted after attempt %d: %w (last error: %v)", attempt, sleepErr, lastErr)
			}
			continue
		}

		verdict := Done
		if opts.OnResult != nil {
			verdict = opts.OnResult(output, at)
		}
		switch verdict {
		case Done:
			return output, nil
		case Fail:
			return nil, newResultError(opts, attempt, output, false)
		default: // Retry
			if attempt >= p.MaxAttempts {
				return nil, newResultError(opts, attempt, output, true)
			}
			backoff, backoffErr := fibonacciBackoffWithJitter(attempt, p.InitialBackoff, p.MaxBackoff, p.Jitter)
			if backoffErr != nil {
				return nil, fmt.Errorf("retry aborted after attempt %d: %w", attempt, backoffErr)
			}
			if sleepErr := sleepContext(ctx, backoff); sleepErr != nil {
				return nil, fmt.Errorf("retry interrupted after attempt %d: %w", attempt, sleepErr)
			}
		}
	}
	return nil, lastErr
}

// fibonacciBackoff returns the nth Fibonacci number × initialBackoff, capped at
// maxBackoff. The series is computed iteratively rather than via Binet's
// formula so a long attempt sequence cannot overflow before the cap applies.
func fibonacciBackoff(attempt int, initialBackoff, maxBackoff time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if initialBackoff <= 0 {
		return 0
	}
	if initialBackoff >= maxBackoff {
		return maxBackoff
	}

	// Stop multiplying as soon as the next term would exceed the cap, which
	// keeps the accumulator bounded by maxBackoff/initialBackoff.
	limit := int64(maxBackoff / initialBackoff)
	previous, current := int64(1), int64(1)
	for n := 3; n <= attempt; n++ {
		if current > limit-previous {
			return maxBackoff
		}
		previous, current = current, previous+current
	}
	return time.Duration(current) * initialBackoff
}

// fibonacciBackoffWithJitter applies downward jitter to fibonacciBackoff.
// jitterFactor is clamped to [0,1]:
//   - 0 returns the base delay unchanged.
//   - 1 returns a random delay in [0, base].
//
// It returns an error only when the entropy source fails while drawing jitter;
// callers must not treat a failed draw as zero jitter.
func fibonacciBackoffWithJitter(attempt int, initialBackoff, maxBackoff time.Duration, jitterFactor float64) (time.Duration, error) {
	base := fibonacciBackoff(attempt, initialBackoff, maxBackoff)
	if base <= 0 {
		return 0, nil
	}
	if jitterFactor <= 0 {
		return base, nil
	}
	if jitterFactor > 1 {
		jitterFactor = 1
	}
	jitterMax := time.Duration(float64(base) * jitterFactor)
	if jitterMax <= 0 {
		return base, nil
	}
	jitter, err := randomDuration(jitterMax)
	if err != nil {
		return 0, err
	}
	delay := base - jitter
	if delay < 0 {
		return 0, nil
	}
	return delay, nil
}

// randomDuration returns a cryptographically random duration in [0, max]. It
// returns an error if the entropy source fails, so callers can surface the
// failure instead of silently substituting zero jitter.
func randomDuration(max time.Duration) (time.Duration, error) {
	if max <= 0 {
		return 0, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)+1))
	if err != nil {
		return 0, fmt.Errorf("draw random duration: %w", err)
	}
	return time.Duration(n.Int64()), nil
}

// sleepContext sleeps for delay, returning early if ctx is cancelled.
func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
