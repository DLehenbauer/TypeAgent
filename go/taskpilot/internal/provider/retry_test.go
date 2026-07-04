package provider

import (
	"testing"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

func TestPolicyFromInputDefaults(t *testing.T) {
	def := retry.Policy{MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: 2 * time.Minute}

	got, err := policyFromInput(map[string]any{}, def)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != def {
		t.Fatalf("defaults = %#v, want %#v", got, def)
	}
}

func TestPolicyFromInputOverrides(t *testing.T) {
	def := retry.Policy{MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: 2 * time.Minute}

	got, err := policyFromInput(map[string]any{
		copilotKeyMaxAttempts:           3,
		copilotKeyInitialBackoffSeconds: 2,
		copilotKeyMaxBackoffSeconds:     10,
	}, def)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d", got.MaxAttempts)
	}
	if got.InitialBackoff != 2*time.Second {
		t.Fatalf("initial backoff = %s", got.InitialBackoff)
	}
	if got.MaxBackoff != 10*time.Second {
		t.Fatalf("max backoff = %s", got.MaxBackoff)
	}
}

func TestPolicyFromInputRejectsMaxBelowInitial(t *testing.T) {
	def := retry.Policy{MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: 2 * time.Minute}

	_, err := policyFromInput(map[string]any{
		copilotKeyInitialBackoffSeconds: 10,
		copilotKeyMaxBackoffSeconds:     5,
	}, def)
	if err == nil {
		t.Fatal("expected error when maxBackoff is below initialBackoff")
	}
}

func TestPolicyFromInputRejectsInvalidOverrides(t *testing.T) {
	def := retry.Policy{MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: 2 * time.Minute}

	cases := []struct {
		name string
		in   map[string]any
	}{
		{"non-positive maxAttempts", map[string]any{copilotKeyMaxAttempts: 0}},
		{"negative maxAttempts", map[string]any{copilotKeyMaxAttempts: -1}},
		{"non-integer maxAttempts", map[string]any{copilotKeyMaxAttempts: "lots"}},
		{"fractional initialBackoff", map[string]any{copilotKeyInitialBackoffSeconds: 1.5}},
		{"non-positive maxBackoff", map[string]any{copilotKeyMaxBackoffSeconds: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := policyFromInput(tc.in, def); err == nil {
				t.Fatalf("expected error for %v", tc.in)
			}
		})
	}
}
