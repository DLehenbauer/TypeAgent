package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// JsonlStringifyInput is the input contract for the jsonl.stringify task.
// Items is the required list of values to serialize; each item is JSON-encoded
// onto its own line, producing JSONL output.
type JsonlStringifyInput struct {
	Items []any `json:"items"`
}

var jsonlStringifySpec = model.TaskSpec{
	Name:        "jsonl.stringify",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(JsonlStringifyInput{})),
}

func stringifyJSONL(_ context.Context, input map[string]any, _ Context) (any, error) {
	var b strings.Builder
	for _, item := range asSlice(input["items"]) {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		b.Write(raw)
		b.WriteString(jsonlLineSeparator)
	}
	return b.String(), nil
}
