package builtin

import (
	"context"
	"fmt"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
)

// copilotInvokeInput defines the JSON-schema shape for copilot.invoke. Prompt is
// required; other fields are provider options for model/context, output
// validation, execution scope, idle timeout, and retry tuning.
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

// copilotTask adapts the Copilot provider to the builtin Task interface.
type copilotTask struct{ BaseTask }

// Spec returns the copilot.invoke metadata used for registration and validation.
func (t *copilotTask) Spec() model.TaskSpec {
	return model.TaskSpec{
		Name:        "copilot.invoke",
		Version:     "1",
		InputSchema: structToSchema(reflect.TypeOf(copilotInvokeInput{})),
	}
}

// Execute submits a Copilot request, or returns a planned result during dry runs.
func (t *copilotTask) Execute(ctx context.Context, input map[string]any, taskCtx Context) (any, error) {
	if taskCtx.DryRun {
		policy, err := provider.CopilotPolicy(input)
		if err != nil {
			return nil, err
		}
		return map[string]any{"planned": true, "prompt": input["prompt"], "context": input["context"], "retry": policy}, nil
	}
	if taskCtx.Services == nil || taskCtx.Services.Providers == nil {
		return nil, fmt.Errorf("copilot provider not configured")
	}
	p, ok := taskCtx.Services.Providers.Get(provider.NameCopilot)
	if !ok {
		return nil, fmt.Errorf("copilot provider not configured")
	}
	return p.Submit(ctx, provider.Request{Input: input}).Await(ctx)
}
