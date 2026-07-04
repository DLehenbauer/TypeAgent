package builtin

import (
	"context"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
)

func TestRunIDReturnsContextRunID(t *testing.T) {
	got, err := runID(context.Background(), map[string]any{}, Context{RunID: "run-42"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "run-42" {
		t.Fatalf("runID = %v, want %q", got, "run-42")
	}
}

func TestRunIDInputSchemaRejectsProperties(t *testing.T) {
	// An empty object is the only accepted input.
	if err := schema.Validate(runIDSpec.InputSchema, map[string]any{}); err != nil {
		t.Fatalf("empty input should validate: %v", err)
	}
	// Any property must be rejected rather than silently ignored.
	if err := schema.Validate(runIDSpec.InputSchema, map[string]any{"extra": 1}); err == nil {
		t.Fatal("non-empty input should be rejected")
	}
}
