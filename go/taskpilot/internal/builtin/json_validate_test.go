package builtin

import (
	"context"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
)

func TestJSONValidateInputSchemaBoundsSchemaField(t *testing.T) {
	// An object schema is the only valid schema form in taskpilot, so it must
	// pass the input boundary.
	ok := map[string]any{
		jsonValidateValueField:  1,
		jsonValidateSchemaField: map[string]any{"type": "number"},
	}
	if err := schema.Validate(jsonValidateSpec.InputSchema, ok); err != nil {
		t.Fatalf("object schema should validate at the boundary: %v", err)
	}

	// Non-object schema forms are rejected at the input boundary rather than
	// deep inside compilation.
	for _, bad := range []any{true, false, "string schema", 42, []any{"array"}} {
		in := map[string]any{
			jsonValidateValueField:  1,
			jsonValidateSchemaField: bad,
		}
		if err := schema.Validate(jsonValidateSpec.InputSchema, in); err == nil {
			t.Fatalf("non-object schema %#v should be rejected at the boundary", bad)
		}
	}
}

func TestValidateJSONReportsResult(t *testing.T) {
	out, err := validateJSON(context.Background(), map[string]any{
		jsonValidateSchemaField: map[string]any{"type": "number"},
		jsonValidateValueField:  1,
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m[jsonValidateValidField] != true {
		t.Fatalf("expected valid=true, got %#v", m)
	}

	out, err = validateJSON(context.Background(), map[string]any{
		jsonValidateSchemaField: map[string]any{"type": "number"},
		jsonValidateValueField:  "not a number",
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	m = out.(map[string]any)
	if m[jsonValidateValidField] != false {
		t.Fatalf("expected valid=false, got %#v", m)
	}
	if _, ok := m[jsonValidateErrorField]; !ok {
		t.Fatalf("expected an error message, got %#v", m)
	}
}
