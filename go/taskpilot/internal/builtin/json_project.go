package builtin

import (
	"context"
	"fmt"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

type jsonProjectInput struct {
	Value any      `json:"value"`
	Path  []string `json:"path"`
}

func jsonProjectSpec() model.TaskSpec {
	return model.TaskSpec{
		Name:        "json.project",
		Version:     "1",
		InputSchema: structToSchema(reflect.TypeOf(jsonProjectInput{})),
	}
}

// projectJSON follows object keys from Value according to Path.
func projectJSON(_ context.Context, input map[string]any, _ Context) (any, error) {
	in, err := decodeInput[jsonProjectInput](input)
	if err != nil {
		return nil, err
	}
	v, ok := lookup(in.Value, in.Path)
	if !ok {
		return nil, fmt.Errorf("path %v not found", in.Path)
	}
	return v, nil
}
