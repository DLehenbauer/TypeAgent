package builtin

import (
	"context"
	"fmt"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
)

// copilotInvokeInput is the validated input contract for the copilot.invoke
// task. Prompt is required; all other fields are optional and fall back to
// provider defaults when omitted. Context carries arbitrary data passed to the
// model, and OutputSchema, when set, constrains the model's response shape.
// Model, ReasoningEffort, and ContextTier select the model and its effort/tier.
// WorkingDirectory and PermissionMode scope filesystem and permission behavior.
// ExpectJson signals that the response must be JSON. SessionIdleTimeoutSeconds
// bounds session idle time. MaxAttempts, InitialBackoffSeconds, and
// MaxBackoffSeconds govern retry pacing, and MaxValidationAttempts caps
// output-schema validation retries. Every integer knob is validated up front:
// when present it must be a positive integer (and MaxBackoffSeconds must not
// fall below the effective InitialBackoffSeconds), otherwise the task fails
// rather than silently defaulting or clamping the illegal value.
type copilotInvokeInput struct {
	Prompt                    string `json:"prompt"`
	Context                   any    `json:"context,omitempty"`
	OutputSchema              any    `json:"outputSchema,omitempty"`
	Model                     string `json:"model,omitempty"`
	ReasoningEffort           string `json:"reasoningEffort,omitempty"`
	ContextTier               string `json:"contextTier,omitempty"`
	WorkingDirectory          string `json:"workingDirectory,omitempty"`
	PermissionMode            string `json:"permissionMode,omitempty"`
	ExpectJson                bool   `json:"expectJson,omitempty"`
	SessionIdleTimeoutSeconds int    `json:"sessionIdleTimeoutSeconds,omitempty"`
	MaxAttempts               int    `json:"maxAttempts,omitempty"`
	InitialBackoffSeconds     int    `json:"initialBackoffSeconds,omitempty"`
	MaxBackoffSeconds         int    `json:"maxBackoffSeconds,omitempty"`
	MaxValidationAttempts     int    `json:"maxValidationAttempts,omitempty"`
}

type copilotTask struct{ BaseTask }

func (t *copilotTask) Spec() model.TaskSpec {
	return model.TaskSpec{
		Name:        "copilot.invoke",
		Version:     "1",
		InputSchema: structToSchema(reflect.TypeOf(copilotInvokeInput{})),
	}
}

func (t *copilotTask) Execute(ctx context.Context, input map[string]any, taskCtx Context) (any, error) {
	if taskCtx.DryRun {
		policy, err := provider.CopilotPolicy(input)
		if err != nil {
			return nil, err
		}
		return map[string]any{"planned": true, "prompt": input["prompt"], "context": input["context"], "retry": policy}, nil
	}
	p, ok := taskCtx.Providers.Get(provider.NameCopilot)
	if !ok {
		return nil, fmt.Errorf("copilot provider not configured")
	}
	return p.Submit(ctx, provider.Request{Input: input}).Await(ctx)
}
