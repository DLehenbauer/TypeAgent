package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// jsonlLineSeparator delimits records in the JSONL format shared by
// jsonl.parse and jsonl.stringify.
const jsonlLineSeparator = "\n"

// JsonlParseInput is the input contract for jsonl.parse. Text is the required
// newline-separated JSONL payload; each non-empty line must be a valid JSON
// value, and blank lines are ignored.
type JsonlParseInput struct {
	Text string `json:"text"`
}

var jsonlParseSpec = model.TaskSpec{
	Name:        "jsonl.parse",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(JsonlParseInput{})),
}

func parseJSONL(_ context.Context, input map[string]any, _ Context) (any, error) {
	text, ok := input["text"].(string)
	if !ok {
		return nil, fmt.Errorf("jsonl.parse requires string %q input, got %T", "text", input["text"])
	}
	lines := strings.Split(text, jsonlLineSeparator)
	var out []any
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, fmt.Errorf("jsonl line %d: %w", i+1, err)
		}
		out = append(out, item)
	}
	return out, nil
}
