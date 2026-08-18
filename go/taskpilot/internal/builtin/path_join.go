package builtin

import (
	"context"
	"path/filepath"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// PathJoinInput is the input for the path.join task. Parts contains the path
// segments joined in order via filepath.Join.
type PathJoinInput struct {
	Parts []string `json:"parts"`
}

func pathJoinSpec() model.TaskSpec {
	return model.TaskSpec{
		Name:        "path.join",
		Version:     "1",
		InputSchema: structToSchema(reflect.TypeOf(PathJoinInput{})),
	}
}

func joinPath(_ context.Context, input map[string]any, _ Context) (any, error) {
	in, err := decodeInput[PathJoinInput](input)
	if err != nil {
		return nil, err
	}
	return filepath.Join(in.Parts...), nil
}
