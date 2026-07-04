package provider

import (
	"fmt"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

// policyFromInput builds a retry.Policy from the common per-node knobs
// (maxAttempts, initialBackoffSeconds, maxBackoffSeconds), falling back to def
// for any knob that is absent. These are the same input fields the workflow
// author sets on a node; the provider now owns how they drive transient-error
// retry.
//
// A knob that is present but not a positive integer is a configuration error,
// not something to paper over: policyFromInput returns an error rather than
// silently reverting to the default. Likewise, a maxBackoffSeconds that lands
// below the effective initialBackoff is rejected instead of being silently
// clamped up to it.
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
		p.InitialBackoff = time.Duration(v) * time.Second
	}
	if v, ok, err := positiveOverride(input, copilotKeyMaxBackoffSeconds); err != nil {
		return retry.Policy{}, err
	} else if ok {
		p.MaxBackoff = time.Duration(v) * time.Second
	}
	if p.MaxBackoff < p.InitialBackoff {
		return retry.Policy{}, fmt.Errorf("retry override %q (%s) must be >= %q (%s)",
			copilotKeyMaxBackoffSeconds, p.MaxBackoff, copilotKeyInitialBackoffSeconds, p.InitialBackoff)
	}
	return p, nil
}

// positiveOverride reads a per-node retry knob. It reports (0, false, nil) when
// the key is absent (use the default), (v, true, nil) for a valid positive
// integer, and an error when the key is present but not a positive integer.
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
