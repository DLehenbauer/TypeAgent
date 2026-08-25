package provider

import (
	"fmt"
	"math"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

// policyFromInput builds a retry policy from the node's override map and a
// default policy. Present overrides must be positive integers, and the effective
// maxBackoffSeconds must be at least initialBackoffSeconds.
func policyFromInput(input map[string]any, def retry.Policy) (retry.Policy, error) {
	p := def
	if v, ok, err := positiveOverride(input, copilotKeyMaxAttempts); err != nil {
		return retry.Policy{}, err
	} else if ok {
		p.MaxAttempts = v
	}
	if v, ok, err := positiveOverride(input, copilotKeyInitialBackoffSeconds); err != nil {
		return retry.Policy{}, err
	} else if ok {
		p.InitialBackoff, err = durationFromSeconds(v, copilotKeyInitialBackoffSeconds)
		if err != nil {
			return retry.Policy{}, err
		}
	}
	if v, ok, err := positiveOverride(input, copilotKeyMaxBackoffSeconds); err != nil {
		return retry.Policy{}, err
	} else if ok {
		p.MaxBackoff, err = durationFromSeconds(v, copilotKeyMaxBackoffSeconds)
		if err != nil {
			return retry.Policy{}, err
		}
	}
	if p.MaxBackoff < p.InitialBackoff {
		return retry.Policy{}, fmt.Errorf("retry override %q (%s) must be >= %q (%s)",
			copilotKeyMaxBackoffSeconds, p.MaxBackoff, copilotKeyInitialBackoffSeconds, p.InitialBackoff)
	}
	return p, nil
}

// durationFromSeconds converts a positive second count from a node override,
// rejecting values that time.Duration cannot represent instead of wrapping into
// a negative delay.
func durationFromSeconds(seconds int, key string) (time.Duration, error) {
	if time.Duration(seconds) > math.MaxInt64/time.Second {
		return 0, fmt.Errorf("override %q: %d seconds exceeds the supported duration", key, seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

// positiveOverride reads a per-node retry knob. It reports (0, false, nil) when
// the key is absent and returns an error when a present value is not a positive
// integer.
func positiveOverride(input map[string]any, key string) (int, bool, error) {
	raw, present := input[key]
	if !present {
		return 0, false, nil
	}
	v, ok := intValue(raw)
	if !ok || v <= 0 {
		return 0, false, fmt.Errorf("retry override %q: want a positive integer, got %v", key, raw)
	}
	return v, true, nil
}
