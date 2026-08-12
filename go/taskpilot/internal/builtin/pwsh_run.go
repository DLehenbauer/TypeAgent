package builtin

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/script"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

// PwshRunInput configures a pwsh.run: the script and its args run under the
// pwsh provider in cwd, capped at timeoutSeconds. On a successful (exit 0) run
// the script must print a JSON value on stdout, which is parsed into result;
// non-zero exits carry stdout through unparsed. DryRunStdout supplies that
// success-path stdout offline so a dry run plans without executing. Likewise,
// the retry/exit fields decide whether a run is
// done, retried, or failed: retryableExitCodes and retryOnTimeout schedule
// another attempt, allowNonZeroExit accepts any non-zero exit as a branchable
// result, and maxAttempts/initialBackoffSeconds/maxBackoffSeconds tune backoff.
type PwshRunInput struct {
	Script         string `json:"script"`
	Args           []any  `json:"args,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	DryRunStdout   string `json:"dryRunStdout,omitempty"`
	// RetryableExitCodes lists non-zero exit codes that should be retried (the
	// script asserts these are transient and that it is safe to re-run).
	RetryableExitCodes []int `json:"retryableExitCodes,omitempty"`
	// RetryOnTimeout retries when the script is killed by its timeout.
	RetryOnTimeout bool `json:"retryOnTimeout,omitempty"`
	// AllowNonZeroExit treats any non-zero exit as a successful (cacheable)
	// result the workflow can branch on, instead of a node failure.
	AllowNonZeroExit bool `json:"allowNonZeroExit,omitempty"`
	// RunsOn optionally binds a lease, running the script inside that execution
	// context instead of on the host. Host execution is simply the absence of a
	// lease, so an unbound RunsOn preserves today's behaviour exactly. The name
	// matches GitHub Actions' runs-on and the engine's own dependsOn; `on` was
	// avoided because it means triggers in Actions.
	RunsOn any `json:"runsOn,omitempty"`
	// Cache controls host-side memoization. It defaults to true when omitted.
	// Lease-bound runs are always executed regardless of this value.
	Cache bool `json:"cache,omitempty"`
	// Checkpoint asks a target-bound run to commit the successful result as the
	// next lease state. It is invalid without RunsOn.
	Checkpoint            bool `json:"checkpoint,omitempty"`
	MaxAttempts           int  `json:"maxAttempts,omitempty"`
	InitialBackoffSeconds int  `json:"initialBackoffSeconds,omitempty"`
	MaxBackoffSeconds     int  `json:"maxBackoffSeconds,omitempty"`
}

type pwshTask struct{}

// pwshRunTaskName is the registered name of the pwsh.run builtin. It is a
// constant so the external-state policy table can reference it without an
// initialization cycle through Spec().
const pwshRunTaskName = "pwsh.run"

// PwshRunsOnInput is the input key binding a lease to a pwsh.run node.
const PwshRunsOnInput = "runsOn"

func (t *pwshTask) Spec() model.TaskSpec {
	return model.TaskSpec{
		Name:        pwshRunTaskName,
		Version:     "1",
		InputSchema: structToSchema(reflect.TypeOf(PwshRunInput{})),
		LeaseInputs: []string{PwshRunsOnInput},
		EmitsLease:  true,
	}
}

func (t *pwshTask) Execute(ctx context.Context, input map[string]any, taskCtx Context) (any, error) {
	lease, onLease, err := optionalLeaseInput(input, PwshRunsOnInput, pwshRunTaskName)
	if err != nil {
		return nil, err
	}
	if !onLease && boolValue(input["checkpoint"]) {
		return nil, fmt.Errorf("%s: checkpoint requires %s", pwshRunTaskName, PwshRunsOnInput)
	}
	if taskCtx.DryRun {
		stdout := asString(input["dryRunStdout"])
		result, err := script.ParseStdout(stdout)
		if err != nil {
			return nil, err
		}
		out := map[string]any{provider.PwshOutputResult: result, provider.PwshOutputStdout: stdout, provider.PwshOutputStderr: "", provider.PwshOutputExitCode: 0, "planned": true}
		if onLease {
			out[LeaseOutput] = model.LeaseRef(lease)
		}
		return out, nil
	}
	if onLease {
		return t.executeOnLease(ctx, lease, input, taskCtx)
	}
	if taskCtx.Services == nil || taskCtx.Services.Providers == nil {
		return nil, fmt.Errorf("pwsh provider not configured")
	}
	p, ok := taskCtx.Services.Providers.Get(provider.NamePwsh)
	if !ok {
		return nil, fmt.Errorf("pwsh provider not configured")
	}
	return p.Submit(ctx, provider.Request{Input: input}).Await(ctx)
}

// executeOnLease runs the script inside the leased execution context and emits
// the successor lease.
//
// Only a checkpointing run advances the lease's state. A transient run threads
// the lease through unchanged.
func (t *pwshTask) executeOnLease(ctx context.Context, lease model.Lease, input map[string]any, taskCtx Context) (any, error) {
	if taskCtx.Services == nil || taskCtx.Services.Targets == nil {
		return nil, fmt.Errorf("%s: execution targets are not configured", pwshRunTaskName)
	}
	backend, err := taskCtx.Services.Targets.Require(target.Kind(lease.Kind))
	if err != nil {
		return nil, err
	}
	checkpoint := boolValue(input["checkpoint"])
	request, err := script.Decode(input, model.FileRefPath)
	if err != nil {
		return nil, err
	}
	req := target.RunRequest{ID: lease.ID, State: lease.State, Script: request}
	if checkpoint {
		// The node's own content-addressed identity is the durable name of the
		// state this run establishes, so the provider can key its store by it and
		// recognize the same state on a later run.
		req.Checkpoint = taskCtx.NodeID
	}
	outcome, err := backend.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	out := outcome.Result.Map()
	successor := lease
	// Advance only on a confirmed commit. A body can fail without the call
	// failing (a non-zero exit is an ordinary result, and allowNonZeroExit makes
	// it a success), and naming a state the provider never snapshotted would
	// fail unrecoverably at the next restore.
	if checkpoint && outcome.Committed {
		successor = lease.WithState(taskCtx.NodeID)
	}
	out[LeaseOutput] = model.LeaseRef(successor)
	return out, nil
}

// Retry classifies each run by its result. A malformed provider result (not the
// expected result map, or one lacking a parseable exitCode) is rejected rather
// than mistaken for success. Otherwise a zero exit is accepted; a listed
// retryableExitCode (or a timeout when retryOnTimeout is set) schedules another
// attempt; any other non-zero exit is a failure unless allowNonZeroExit makes it
// an accepted result. Because a failure is surfaced as an error, failed runs are
// never cached.
func (t *pwshTask) Retry(input map[string]any) (retry.Options, error) {
	policy, err := pwshPolicy(input)
	if err != nil {
		return retry.Options{}, err
	}
	codes := intSet(input["retryableExitCodes"])
	retryOnTimeout := boolValue(input["retryOnTimeout"])
	allowNonZero := boolValue(input["allowNonZeroExit"])
	return retry.Options{
		Policy:         policy,
		DescribeResult: describePwshFailure,
		OnResult: func(out any, _ retry.Attempt) retry.Verdict {
			m, ok := out.(map[string]any)
			if !ok {
				return retry.Fail
			}
			if boolValue(m[provider.PwshOutputTimedOut]) {
				if retryOnTimeout {
					return retry.Retry
				}
				return retry.Fail
			}
			code, ok := intValue(m[provider.PwshOutputExitCode])
			if !ok {
				return retry.Fail
			}
			switch {
			case code == 0:
				return retry.Done
			case codes[code]:
				return retry.Retry
			case allowNonZero:
				return retry.Done
			default:
				return retry.Fail
			}
		},
	}, nil
}

// describePwshFailure summarizes a rejected pwsh result for the error message:
// whether it timed out, which non-zero code it exited with, or that the provider
// returned a malformed result (not the expected map, or one lacking a parseable
// exitCode), plus a trimmed snippet of stderr (where scripts write the actual
// reason). It is the seam that turns an opaque "result rejected" into an
// actionable diagnostic.
func describePwshFailure(out any) string {
	m, ok := out.(map[string]any)
	if !ok {
		return fmt.Sprintf("pwsh provider returned a malformed result of type %T", out)
	}
	var b strings.Builder
	switch {
	case boolValue(m[provider.PwshOutputTimedOut]):
		b.WriteString("pwsh script timed out")
	default:
		if code, ok := intValue(m[provider.PwshOutputExitCode]); ok {
			fmt.Fprintf(&b, "pwsh script exited with code %d", code)
		} else {
			b.WriteString("pwsh result has a missing or malformed exitCode")
		}
	}
	if stderr := truncateTail(strings.TrimSpace(asString(m[provider.PwshOutputStderr])), provider.StderrTailCap); stderr != "" {
		b.WriteString(": ")
		b.WriteString(stderr)
	}
	return b.String()
}

// truncateTail keeps the last max bytes of s (scripts emit the operative error
// last), prefixing an ellipsis when content was dropped.
func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}

func pwshPolicy(input map[string]any) (retry.Policy, error) {
	p := retry.DefaultPolicy()
	if v, ok := intValue(input["maxAttempts"]); ok && v > 0 {
		p.MaxAttempts = v
	}
	if v, ok := intValue(input["initialBackoffSeconds"]); ok && v > 0 {
		p.InitialBackoff = time.Duration(v) * time.Second
	}
	if v, ok := intValue(input["maxBackoffSeconds"]); ok && v > 0 {
		p.MaxBackoff = time.Duration(v) * time.Second
	}
	if p.MaxBackoff < p.InitialBackoff {
		return retry.Policy{}, fmt.Errorf("pwsh.run: maxBackoffSeconds (%s) must not be less than initialBackoffSeconds (%s)", p.MaxBackoff, p.InitialBackoff)
	}
	return p, nil
}
