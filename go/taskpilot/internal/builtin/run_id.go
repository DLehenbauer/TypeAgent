package builtin

import (
	"context"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// RunIDTaskName is the canonical task name for the run.id builtin.
const RunIDTaskName = "run.id"

var runIDSpec = model.TaskSpec{
	Name:    RunIDTaskName,
	Version: "1",
	// run.id takes no input; reject any properties rather than silently
	// ignoring them.
	InputSchema: map[string]any{"type": "object", "additionalProperties": false},
}

func runID(_ context.Context, _ map[string]any, ctx Context) (any, error) {
	return ctx.RunID, nil
}
