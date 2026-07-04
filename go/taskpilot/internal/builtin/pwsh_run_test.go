package builtin

import (
	"strings"
	"testing"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

func TestPwshRetryClassification(t *testing.T) {
	cases := []struct {
		name   string
		input  map[string]any
		result any
		want   retry.Verdict
	}{
		{"zero exit is done", map[string]any{}, map[string]any{provider.PwshOutputExitCode: 0}, retry.Done},
		{"non-zero fails by default", map[string]any{}, map[string]any{provider.PwshOutputExitCode: 1}, retry.Fail},
		{"retryable exit retries", map[string]any{"retryableExitCodes": []any{75}}, map[string]any{provider.PwshOutputExitCode: 75}, retry.Retry},
		{"non-listed non-zero fails", map[string]any{"retryableExitCodes": []any{75}}, map[string]any{provider.PwshOutputExitCode: 2}, retry.Fail},
		{"allowNonZeroExit accepts", map[string]any{"allowNonZeroExit": true}, map[string]any{provider.PwshOutputExitCode: 3}, retry.Done},
		{"timeout fails by default", map[string]any{}, map[string]any{provider.PwshOutputExitCode: 1, provider.PwshOutputTimedOut: true}, retry.Fail},
		{"timeout retries when enabled", map[string]any{"retryOnTimeout": true}, map[string]any{provider.PwshOutputExitCode: 1, provider.PwshOutputTimedOut: true}, retry.Retry},
		{"non-map result fails", map[string]any{"allowNonZeroExit": true}, "not a map", retry.Fail},
		{"missing exit code fails", map[string]any{"allowNonZeroExit": true}, map[string]any{provider.PwshOutputStdout: "hi"}, retry.Fail},
		{"unparseable exit code fails", map[string]any{"allowNonZeroExit": true}, map[string]any{provider.PwshOutputExitCode: "oops"}, retry.Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := (&pwshTask{}).Retry(tc.input)
			if err != nil {
				t.Fatalf("Retry returned error: %v", err)
			}
			if opts.OnResult == nil {
				t.Fatal("pwsh Retry has no OnResult hook")
			}
			if got := opts.OnResult(tc.result, retry.Attempt{Num: 1}); got != tc.want {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPwshPolicyDefaultsAndOverrides(t *testing.T) {
	def, err := pwshPolicy(map[string]any{})
	if err != nil {
		t.Fatalf("default policy error: %v", err)
	}
	if def.MaxAttempts != 1 {
		t.Fatalf("default max attempts = %d, want 1", def.MaxAttempts)
	}

	over, err := pwshPolicy(map[string]any{
		"maxAttempts":           3,
		"initialBackoffSeconds": 2,
		"maxBackoffSeconds":     10,
	})
	if err != nil {
		t.Fatalf("override policy error: %v", err)
	}
	if over.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d", over.MaxAttempts)
	}
	if over.InitialBackoff != 2*time.Second {
		t.Fatalf("initial backoff = %s", over.InitialBackoff)
	}
	if over.MaxBackoff != 10*time.Second {
		t.Fatalf("max backoff = %s", over.MaxBackoff)
	}
}

func TestPwshPolicyRejectsMaxBackoffBelowInitial(t *testing.T) {
	if _, err := pwshPolicy(map[string]any{
		"initialBackoffSeconds": 10,
		"maxBackoffSeconds":     5,
	}); err == nil {
		t.Fatal("expected error when maxBackoffSeconds is below initialBackoffSeconds")
	}
	if _, err := (&pwshTask{}).Retry(map[string]any{
		"initialBackoffSeconds": 10,
		"maxBackoffSeconds":     5,
	}); err == nil {
		t.Fatal("expected Retry to surface invalid backoff configuration")
	}
}

func TestBaseTaskRetryIsNoRetry(t *testing.T) {
	opts, err := BaseTask{}.Retry(map[string]any{})
	if err != nil {
		t.Fatalf("BaseTask.Retry error: %v", err)
	}
	if opts.OnError != nil || opts.OnResult != nil || opts.Policy.MaxAttempts != 0 {
		t.Fatalf("BaseTask.Retry = %#v, want zero retry.Options", opts)
	}
}

func TestDescribePwshFailure(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]any
		want   string
	}{
		{"exit code with stderr", map[string]any{provider.PwshOutputExitCode: 1, provider.PwshOutputStderr: "preflight: working tree is not clean\n"}, "pwsh script exited with code 1: preflight: working tree is not clean"},
		{"exit code no stderr", map[string]any{provider.PwshOutputExitCode: 2}, "pwsh script exited with code 2"},
		{"timeout takes precedence", map[string]any{provider.PwshOutputExitCode: 1, provider.PwshOutputTimedOut: true, provider.PwshOutputStderr: "boom"}, "pwsh script timed out: boom"},
		{"over-cap stderr is tail-truncated", map[string]any{provider.PwshOutputExitCode: 1, provider.PwshOutputStderr: strings.Repeat("x", provider.StderrTailCap+50)}, "pwsh script exited with code 1: ..." + strings.Repeat("x", provider.StderrTailCap)},
		{"missing exit code is malformed", map[string]any{provider.PwshOutputStdout: "hi"}, "pwsh result has a missing or malformed exitCode"},
		{"unparseable exit code with stderr", map[string]any{provider.PwshOutputExitCode: "oops", provider.PwshOutputStderr: "why"}, "pwsh result has a missing or malformed exitCode: why"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describePwshFailure(tc.result); got != tc.want {
				t.Fatalf("describePwshFailure = %q, want %q", got, tc.want)
			}
		})
	}
	if got := describePwshFailure("not a map"); got != "pwsh provider returned a malformed result of type string" {
		t.Fatalf("describePwshFailure(non-map) = %q", got)
	}
}

func TestPwshRetryDescribesRejectedResult(t *testing.T) {
	opts, err := (&pwshTask{}).Retry(map[string]any{})
	if err != nil {
		t.Fatalf("Retry returned error: %v", err)
	}
	if opts.DescribeResult == nil {
		t.Fatal("pwsh Retry has no DescribeResult hook")
	}
	got := opts.DescribeResult(map[string]any{provider.PwshOutputExitCode: 1, provider.PwshOutputStderr: "the reason"})
	if got != "pwsh script exited with code 1: the reason" {
		t.Fatalf("DescribeResult = %q", got)
	}
}

func TestTruncateTail(t *testing.T) {
	if got := truncateTail("short", provider.StderrTailCap); got != "short" {
		t.Fatalf("truncateTail short = %q", got)
	}
	got := truncateTail("abcdef", 3)
	if got != "...def" {
		t.Fatalf("truncateTail = %q, want ...def", got)
	}
}
