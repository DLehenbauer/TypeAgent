package builtin

import (
	"context"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
)

// Field names used by the json.validate input schema and handler result.
const (
	jsonValidateValueField  = "value"
	jsonValidateSchemaField = "schema"
	jsonValidateValidField  = "valid"
	jsonValidateErrorField  = "error"
)

var jsonValidateSpec = model.TaskSpec{
	Name:    "json.validate",
	Version: "1",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			jsonValidateValueField: map[string]any{},
			// taskpilot schemas are always JSON objects (see internal/schema);
			// bound the field here so a non-object schema is rejected at the
			// input boundary instead of deep inside compilation.
			jsonValidateSchemaField: map[string]any{"type": "object"},
		},
		"required": []string{jsonValidateValueField, jsonValidateSchemaField},
	},
}

// validateJSON validates input["value"] against input["schema"] and returns
// "valid", plus "error" when validation fails.
func validateJSON(_ context.Context, input map[string]any, _ Context) (any, error) {
	err := schema.Validate(input[jsonValidateSchemaField], input[jsonValidateValueField])
	out := map[string]any{jsonValidateValidField: err == nil}
	if err != nil {
		out[jsonValidateErrorField] = err.Error()
	}
	return out, nil
}
