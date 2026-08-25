package builtin

import (
	"context"
	"fmt"
	"os"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// fileWriteInput is the input for file.write. The destination path is created
// or truncated and written with mode 0666 under the current umask.
type fileWriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

var fileWriteSpec = model.TaskSpec{
	Name:        "file.write",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(fileWriteInput{})),
	AlwaysRun:   true,
}

// writeFile writes content to path and returns the written path. Dry runs
// validate input but leave the file untouched.
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
