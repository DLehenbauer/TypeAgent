package builtin

import (
	"context"
	"reflect"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// StringJoinInput is the input contract for the string.join task. List holds
// the ordered elements to concatenate and Delimiter is inserted between each
// adjacent pair; both fields are required.
type StringJoinInput struct {
	List      []string `json:"list"`
	Delimiter string   `json:"delimiter"`
}

var stringJoinSpec = model.TaskSpec{
	Name:        "string.join",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(StringJoinInput{})),
}

func joinString(_ context.Context, input map[string]any, _ Context) (any, error) {
	in, err := decodeInput[StringJoinInput](input)
	if err != nil {
		return nil, err
	}
	return strings.Join(in.List, in.Delimiter), nil
}
