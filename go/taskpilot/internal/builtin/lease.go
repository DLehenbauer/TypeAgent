package builtin

import (
	"context"
	"fmt"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

// Task names and input keys for the lease lifecycle builtins. One acquire/
// release pair serves every execution-context kind: the kind is data, so adding
// SSH or container support later adds a provider registration and no builtins.
const (
	// LeaseAcquireTaskName mints a lease, rooting a chain.
	LeaseAcquireTaskName = "lease.acquire"
	// LeaseReleaseTaskName consumes a lease without emitting a successor,
	// ending a chain.
	LeaseReleaseTaskName = "lease.release"

	// LeaseKindInput selects which execution-context backend serves the lease.
	LeaseKindInput = "kind"
	// LeaseOptionsInput carries kind-specific acquisition settings.
	LeaseOptionsInput = "options"
	// LeaseKeepOnFailureInput controls cleanup if the graph exits before the
	// terminal release node. It does not define target state and is excluded
	// from node identity.
	LeaseKeepOnFailureInput = "keepOnFailure"
	// LeaseInput binds the lease a terminal task consumes.
	LeaseInput = "lease"
	// LeaseKeepInput asks release to relinquish the engine's claim without
	// tearing the context down, so a developer can inspect it afterwards. The
	// node is still present and verification still passes, which is what keeps
	// the terminal-release rule from being too rigid to express "leave it up".
	LeaseKeepInput = "keep"

	// LeaseOutput names the successor lease a lease-threading task emits. It is
	// the same spelling verification uses to recognize a lease-carrying
	// reference, so the two cannot drift.
	LeaseOutput = model.LeaseOutputKey
)

// leaseAcquireTask acquires a target lease and publishes it for downstream tasks.
type leaseAcquireTask struct{ BaseTask }

// Spec returns the lease.acquire metadata exposed by the builtin registry.
func (t *leaseAcquireTask) Spec() model.TaskSpec {
	return model.TaskSpec{
		Name:       LeaseAcquireTaskName,
		Version:    "1",
		EmitsLease: true,
		AlwaysRun:  true,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{LeaseKindInput},
			"properties": map[string]any{
				LeaseKindInput:          map[string]any{"type": "string"},
				LeaseOptionsInput:       map[string]any{"type": "object"},
				LeaseKeepOnFailureInput: map[string]any{"type": "boolean"},
			},
		},
	}
}

// Execute acquires a lease from the selected target backend.
func (t *leaseAcquireTask) Execute(ctx context.Context, input map[string]any, taskCtx Context) (any, error) {
	kind := target.Kind(asString(input[LeaseKindInput]))
	if kind == "" {
		return nil, fmt.Errorf("%s: %s is required", LeaseAcquireTaskName, LeaseKindInput)
	}
	options, _ := input[LeaseOptionsInput].(map[string]any)
	keepOnFailure := true
	if raw, present := input[LeaseKeepOnFailureInput]; present {
		keepOnFailure = boolValue(raw)
	}

	if taskCtx.DryRun {
		return leaseResult(model.Lease{Kind: string(kind), ID: "dry-run", State: "dry-run"}), nil
	}
	if taskCtx.Services == nil || taskCtx.Services.Targets == nil {
		return nil, fmt.Errorf("%s: execution targets are not configured", LeaseAcquireTaskName)
	}
	backend, err := taskCtx.Services.Targets.Require(kind)
	if err != nil {
		return nil, err
	}
	instance, err := backend.Acquire(ctx, target.AcquireRequest{Options: options, KeepOnFailure: keepOnFailure})
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", LeaseAcquireTaskName, kind, err)
	}
	return leaseResult(model.Lease{Kind: string(kind), ID: instance.ID, State: instance.BaselineState}), nil
}

// leaseReleaseTask releases a target lease, optionally keeping the target for
// inspection.
type leaseReleaseTask struct{ BaseTask }

// Spec returns the lease.release metadata exposed by the builtin registry.
func (t *leaseReleaseTask) Spec() model.TaskSpec {
	return model.TaskSpec{
		Name:        LeaseReleaseTaskName,
		Version:     "1",
		LeaseInputs: []string{LeaseInput},
		AlwaysRun:   true,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{LeaseInput},
			"properties": map[string]any{
				LeaseInput:     map[string]any{"type": "object"},
				LeaseKeepInput: map[string]any{"type": "boolean"},
			},
		},
	}
}

// Execute releases the terminal lease through its target backend.
func (t *leaseReleaseTask) Execute(ctx context.Context, input map[string]any, taskCtx Context) (any, error) {
	lease, ok := model.AsLease(input[LeaseInput])
	if !ok {
		return nil, fmt.Errorf("%s: %s must be a lease", LeaseReleaseTaskName, LeaseInput)
	}
	keep := boolValue(input[LeaseKeepInput])
	if taskCtx.DryRun {
		return map[string]any{"released": !keep}, nil
	}
	if taskCtx.Services == nil || taskCtx.Services.Targets == nil {
		return nil, fmt.Errorf("%s: execution targets are not configured", LeaseReleaseTaskName)
	}
	backend, err := taskCtx.Services.Targets.Require(target.Kind(lease.Kind))
	if err != nil {
		return nil, err
	}
	if err := backend.Release(ctx, lease.ID, keep); err != nil {
		return nil, fmt.Errorf("%s %s: %w", LeaseReleaseTaskName, lease.Kind, err)
	}
	return map[string]any{"released": !keep}, nil
}

// leaseResult wraps an acquired lease under LeaseOutput for downstream bindings.
func leaseResult(l model.Lease) map[string]any {
	return map[string]any{LeaseOutput: model.LeaseRef(l)}
}

// optionalLeaseInput distinguishes host execution from a malformed lease binding.
func optionalLeaseInput(input map[string]any, key, taskName string) (model.Lease, bool, error) {
	raw, present := input[key]
	if !present || raw == nil {
		return model.Lease{}, false, nil
	}
	lease, ok := model.AsLease(raw)
	if !ok {
		return model.Lease{}, false, fmt.Errorf("%s: %s must be a lease; when referencing a lease output, use path: [lease]", taskName, key)
	}
	return lease, true, nil
}
