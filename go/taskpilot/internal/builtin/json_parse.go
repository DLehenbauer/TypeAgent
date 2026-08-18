package builtin

import (
	"context"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// JsonParseInput is the input contract for the json.parse task.
// Text contains the raw JSON value to decode into a single result.
type JsonParseInput struct {
	Text string `json:"text"`
}

var jsonParseSpec = model.TaskSpec{
	Name:        "json.parse",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(JsonParseInput{})),
}

// parseJSON parses input["text"] as JSON.
func parseJSON(_ context.Context, input map[string]any, _ Context) (any, error) {
	return parseJSONString(asString(input["text"]))
}
