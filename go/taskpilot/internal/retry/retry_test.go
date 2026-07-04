package retry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

// noBackoffPolicy returns a deterministic retry policy that never sleeps
// between attempts, so Run-based tests exercise the attempt loop without
// wall-clock delays. Centralizing it here keeps policy-shape changes to a
// single edit site.
func noBackoffPolicy(maxAttempts int) retry.Policy {
	return retry.Policy{MaxAttempts: maxAttempts, InitialBackoff: 0, MaxBackoff: 0}
}

func TestRunSucceedsOnFirstAttempt(t *testing.T) {
	calls := 0
	out, err := retry.Run(context.Background(), retry.Options{Policy: retry.Policy{MaxAttempts: 3}},
		func(_ context.Context) (any, error) {
			calls++
			return "ok", nil
		})
	if err != nil || out != "ok" || calls != 1 {
		t.Fatalf("calls=%d out=%v err=%v", calls, out, err)
	}
}

func TestRunRetriesTransientError(t *testing.T) {
	transient := errors.New("transient")
	calls := 0
	_, err := retry.Run(context.Background(), retry.Options{
		Policy:  noBackoffPolicy(3),
		OnError: func(e error, _ retry.Attempt) bool { return errors.Is(e, transient) },
	}, func(_ context.Context) (any, error) {
		calls++
		return nil, transient
	})
	if err == nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRunStopsOnNonRetryableError(t *testing.T) {
	permanent := errors.New("permanent")
	calls := 0
	_, err := retry.Run(context.Background(), retry.Options{
		Policy:  noBackoffPolicy(5),
		OnError: func(e error, _ retry.Attempt) bool { return false },
	}, func(_ context.Context) (any, error) {
		calls++
		return nil, permanent
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRunPollsUntilResultDone(t *testing.T) {
	calls := 0
	out, err := retry.Run(context.Background(), retry.Options{
		Policy: noBackoffPolicy(3),
		OnResult: func(v any, _ retry.Attempt) retry.Verdict {
			if v.(int) < 3 {
				return retry.Retry
			}
			return retry.Done
		},
	}, func(_ context.Context) (any, error) {
		calls++
		return calls, nil
	})
	if err != nil || out != 3 || calls != 3 {
		t.Fatalf("calls=%d out=%v err=%v", calls, out, err)
	}
}

func TestRunResultRetryExhaustedFails(t *testing.T) {
	calls := 0
	out, err := retry.Run(context.Background(), retry.Options{
		Policy:   noBackoffPolicy(2),
		OnResult: func(any, retry.Attempt) retry.Verdict { return retry.Retry },
	}, func(_ context.Context) (any, error) {
		calls++
		return calls, nil
	})
	if out != nil || calls != 2 {
		t.Fatalf("calls=%d out=%v", calls, out)
	}
	var re *retry.ResultError
	if !errors.As(err, &re) || !re.Exhausted {
		t.Fatalf("err = %v, want exhausted ResultError", err)
	}
	if re.Result != 2 {
		t.Fatalf("ResultError.Result = %v, want last result 2", re.Result)
	}
}

func TestRunResultFailReturnsResultError(t *testing.T) {
	calls := 0
	_, err := retry.Run(context.Background(), retry.Options{
		Policy:   noBackoffPolicy(5),
		OnResult: func(any, retry.Attempt) retry.Verdict { return retry.Fail },
	}, func(_ context.Context) (any, error) {
		calls++
		return "bad", nil
	})
	var re *retry.ResultError
	if !errors.As(err, &re) || re.Exhausted || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if re.Result != "bad" {
		t.Fatalf("ResultError.Result = %v, want bad", re.Result)
	}
}

func TestRunResultFailDescribesResult(t *testing.T) {
	_, err := retry.Run(context.Background(), retry.Options{
		OnResult:       func(any, retry.Attempt) retry.Verdict { return retry.Fail },
		DescribeResult: func(r any) string { return "reason: " + r.(string) },
	}, func(_ context.Context) (any, error) {
		return "boom", nil
	})
	var re *retry.ResultError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want ResultError", err)
	}
	if re.Detail != "reason: boom" {
		t.Fatalf("Detail = %q", re.Detail)
	}
	if got := err.Error(); got != "result rejected after 1 attempt(s): reason: boom" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestRunContextCancelledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the first sleep returns immediately
	transient := errors.New("transient")
	calls := 0
	_, err := retry.Run(ctx, retry.Options{
		Policy:  retry.Policy{MaxAttempts: 5, InitialBackoff: time.Hour, MaxBackoff: time.Hour},
		OnError: func(e error, _ retry.Attempt) bool { return errors.Is(e, transient) },
	}, func(_ context.Context) (any, error) {
		calls++
		return nil, transient
	})
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d (err=%v)", calls, err)
	}
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunDefaultPolicyNoRetry(t *testing.T) {
	calls := 0
	permanent := errors.New("permanent")
	_, err := retry.Run(context.Background(), retry.Options{
		Policy:  retry.DefaultPolicy(),
		OnError: func(e error, _ retry.Attempt) bool { return true }, // even if retryable, MaxAttempts=1 means no retry
	}, func(_ context.Context) (any, error) {
		calls++
		return nil, permanent
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
