package builtin

import (
	"context"
	"fmt"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// ListReduceInput configures the list.reduce task, which folds List into a
// single value. Op selects the reduction: "sum" adds numeric items, "concat"
// joins items as strings, and "append" (or "") collects items into a slice;
// any other Op is rejected. Initial is the starting accumulator; when nil and
// List is non-empty, the first element seeds it and the rest are reduced.
type ListReduceInput struct {
	List    []any  `json:"list"`
	Initial any    `json:"initial,omitempty"`
	Op      string `json:"op,omitempty"`
}

var listReduceSpec = model.TaskSpec{
	Name:        "list.reduce",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(ListReduceInput{})),
}

func reduceList(_ context.Context, input map[string]any, _ Context) (any, error) {
	list := asSlice(input["list"])
	op := asString(input["op"])
	switch op {
	case "concat", "sum", "", "append":
	default:
		return nil, fmt.Errorf("unsupported reduce op %q", op)
	}
	acc := input["initial"]
	if op == "" || op == "append" {
		out := make([]any, 0, len(list)+1)
		if acc != nil {
			out = append(out, asSlice(acc)...)
		}
		return append(out, list...), nil
	}
	if acc == nil && len(list) > 0 {
		acc = list[0]
		list = list[1:]
	}
	for _, item := range list {
		switch op {
		case "concat":
			acc = fmt.Sprint(acc) + fmt.Sprint(item)
		case "sum":
			a, ok := numericValue(acc)
			if !ok {
				return nil, fmt.Errorf("sum reduce requires a numeric accumulator, got %T", acc)
			}
			n, ok := numericValue(item)
			if !ok {
				return nil, fmt.Errorf("sum reduce requires numeric items, got %T", item)
			}
			acc = a + n
		}
	}
	return acc, nil
}
