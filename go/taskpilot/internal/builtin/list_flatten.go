package builtin

import (
	"context"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// ListFlattenInput is the input for the list.flatten task. List is a list of
// lists; flattening concatenates each inner list, removing exactly one level of
// nesting and preserving element order. The input is required.
type ListFlattenInput struct {
	List [][]any `json:"list"`
}

var listFlattenSpec = model.TaskSpec{
	Name:        "list.flatten",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(ListFlattenInput{})),
}

func flattenList(_ context.Context, input map[string]any, _ Context) (any, error) {
	out := make([]any, 0)
	for _, item := range asSlice(input["list"]) {
		out = append(out, asSlice(item)...)
	}
	return out, nil
}
