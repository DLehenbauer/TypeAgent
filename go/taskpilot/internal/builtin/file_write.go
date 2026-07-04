package builtin

import (
	"context"
	"fmt"
	"os"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// fileWriteInput is the input for the file.write task, which writes Content to
// the file at Path. Path is created or truncated and written with 0666 mode
// (subject to umask); both fields are required. On dry-run nothing is written.
type fileWriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

var fileWriteSpec = model.TaskSpec{
	Name:        "file.write",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(fileWriteInput{})),
}

func writeFile(_ context.Context, input map[string]any, ctx Context) (any, error) {
	in, err := decodeInput[fileWriteInput](input)
	if err != nil {
		return nil, err
	}
	if in.Path == "" {
		return nil, fmt.Errorf("file.write: path must not be empty")
	}
	if ctx.DryRun {
		return fmt.Sprintf("DRY-RUN write %s", in.Path), nil
	}
	if err := os.WriteFile(in.Path, []byte(in.Content), 0o666); err != nil {
		return nil, err
	}
	return in.Path, nil
}
