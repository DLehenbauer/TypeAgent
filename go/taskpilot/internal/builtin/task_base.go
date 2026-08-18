package builtin

import (
	"context"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

// Task is the interface for builtin tasks that use the managed retry infrastructure.
// Embed BaseTask in a concrete struct and override Retry only when a task needs
// something other than the conservative no-retry default.
type Task interface {
	// Spec returns the task's schema and metadata for registration and validation.
	Spec() model.TaskSpec
	// Execute runs the builtin task with the provided input and execution context.
	Execute(ctx context.Context, input map[string]any, taskCtx Context) (any, error)
	// Retry returns the retry options that govern this execution, derived from
	// the task's input, or an error when the input configures retry invalidly.
	Retry(input map[string]any) (retry.Options, error)
}

// BaseTask provides conservative no-retry defaults.
type BaseTask struct{}

// Retry returns the conservative no-retry defaults used by builtin tasks unless
// a specific task overrides them.
func (BaseTask) Retry(map[string]any) (retry.Options, error) { return retry.Options{}, nil }

// taskObjects builds a fresh slice of the builtin managed-retry tasks on each
// call, keeping task registration free of shared package state. It is used by
// SchemaRegistry and RuntimeRegistry to register the builtin task set.
func taskObjects() []Task {
	return []Task{
		&copilotTask{},
		&pwshTask{},
		&leaseAcquireTask{},
		&leaseReleaseTask{},
	}
}
