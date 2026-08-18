package builtin

import (
	"context"
	"errors"
	"os"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// FileExistsInput is the input contract for the file.exists task. Path is the
// required filesystem path to test; the task reports true when it exists and
// false when it is absent.
type FileExistsInput struct {
	Path string `json:"path"`
}

var fileExistsSpec = model.TaskSpec{
	Name:        "file.exists",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(FileExistsInput{})),
}

// fileExists executes file.exists, returning false during dry runs without
// touching the filesystem.
func fileExists(_ context.Context, input map[string]any, ctx Context) (any, error) {
	if ctx.DryRun {
		return false, nil
	}
	present, err := filePresent(asString(input["path"]))
	if err != nil {
		return nil, err
	}
	return present, nil
}

// filePresent reports whether path exists. Missing paths return (false, nil);
// other stat errors are propagated.
func filePresent(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
