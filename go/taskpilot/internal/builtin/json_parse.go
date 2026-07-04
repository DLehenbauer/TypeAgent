package builtin

import (
	"context"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// JsonParseInput is the input contract for the json.parse task. Text is the
// required raw JSON string to decode; it must contain a single well-formed JSON
// value, which is returned as the parsed result.
type JsonParseInput struct {
	Text string `json:"text"`
}

var jsonParseSpec = model.TaskSpec{
	Name:        "json.parse",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(JsonParseInput{})),
}

func parseJSON(_ context.Context, input map[string]any, _ Context) (any, error) {
	return parseJSONString(asString(input["text"]))
}
