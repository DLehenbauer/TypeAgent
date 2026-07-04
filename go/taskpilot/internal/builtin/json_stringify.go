package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// JSONStringifyInput is the input for json.stringify. Value is required and may
// be any JSON-serializable value. Indent is optional: a positive value pretty-
// prints with that many spaces per level, while zero or negative produces
// compact output.
type JSONStringifyInput struct {
	Value  any `json:"value"`
	Indent int `json:"indent,omitempty"`
}

var jsonStringifySpec = model.TaskSpec{
	Name:        "json.stringify",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(JSONStringifyInput{})),
}

// stringifyJSON serializes any value to a JSON string. With a positive indent it
// pretty-prints with that many spaces. It is the inverse of json.parse and lets
// a workflow feed structured node output into a string-typed input (e.g.
// file.write content).
func stringifyJSON(_ context.Context, input map[string]any, _ Context) (any, error) {
	in, err := decodeInput[JSONStringifyInput](input)
	if err != nil {
		return nil, err
	}
	if in.Indent > 0 {
		b, err := json.MarshalIndent(in.Value, "", strings.Repeat(" ", in.Indent))
		if err != nil {
			return nil, err
		}
		return string(b), nil
	}
	b, err := json.Marshal(in.Value)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
