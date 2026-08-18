package builtin

import (
	"context"
	"reflect"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// StringSplitInput configures the string.split task. Text is split with
// strings.Split using Delimiter, and KeepEmpty retains empty substrings when set.
type StringSplitInput struct {
	Text      string `json:"text"`
	Delimiter string `json:"delimiter"`
	KeepEmpty bool   `json:"keepEmpty,omitempty"`
}

func stringSplitSpec() model.TaskSpec {
	return model.TaskSpec{
		Name:        "string.split",
		Version:     "1",
		InputSchema: structToSchema(reflect.TypeOf(StringSplitInput{})),
	}
}

func splitString(_ context.Context, input map[string]any, _ Context) (any, error) {
	in, err := decodeInput[StringSplitInput](input)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(in.Text, in.Delimiter)
	if !in.KeepEmpty {
		filtered := parts[:0]
		for _, p := range parts {
			if p != "" {
				filtered = append(filtered, p)
			}
		}
		parts = filtered
	}
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out, nil
}
