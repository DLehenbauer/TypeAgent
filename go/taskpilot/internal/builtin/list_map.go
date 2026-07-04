package builtin

import (
	"context"
	"fmt"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// ListMapInput is the input contract for the list.map task: List holds the
// required source elements to iterate, and Template is the required value
// applied per element (resolving "item"/"index" references) to build the output
// list of the same length.
type ListMapInput struct {
	List     []any `json:"list"`
	Template any   `json:"template"`
}

var listMapSpec = model.TaskSpec{
	Name:        "list.map",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(ListMapInput{})),
}

func mapList(_ context.Context, input map[string]any, _ Context) (any, error) {
	list := asSlice(input["list"])
	template := input["template"]
	out := make([]any, len(list))
	for i, item := range list {
		v, err := applyTemplateValue(template, item, i)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func applyTemplateValue(template any, item any, index int) (any, error) {
	switch t := template.(type) {
	case map[string]any:
		if ref, _ := t["$from"].(string); ref == "item" {
			raw, ok := t["path"]
			if !ok {
				return item, nil
			}
			path, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("item template path must be an array, got %T", raw)
			}
			cur := item
			for _, seg := range path {
				next, found := lookup(cur, []string{fmt.Sprint(seg)})
				if !found {
					return nil, fmt.Errorf("item template path segment %q not found", fmt.Sprint(seg))
				}
				cur = next
			}
			return cur, nil
		}
		if ref, _ := t["$from"].(string); ref == "index" {
			return index, nil
		}
		out := map[string]any{}
		for k, v := range t {
			resolved, err := applyTemplateValue(v, item, index)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			resolved, err := applyTemplateValue(v, item, index)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	default:
		return t, nil
	}
}
