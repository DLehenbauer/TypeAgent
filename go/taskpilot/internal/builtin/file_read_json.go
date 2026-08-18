package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// FileReadJsonInput configures the file.readJson task.
// Path names the JSON file. MaxBytes caps the accepted byte count, defaults to
// 10 MiB when omitted, and must be positive when set.
type FileReadJsonInput struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"maxBytes,omitempty"`
}

var fileReadJSONSpec = model.TaskSpec{
	Name:        "file.readJson",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(FileReadJsonInput{})),
}

// readJSONFile reads and parses a JSON file, enforcing maxBytes before
// decoding.
func readJSONFile(_ context.Context, input map[string]any, ctx Context) (any, error) {
	if ctx.DryRun {
		// Dry-run returns an empty object without reading disk.
		return map[string]any{}, nil
	}
	path := asString(input["path"])
	max := int64(10 * 1024 * 1024)
	if raw, present := input["maxBytes"]; present {
		n, ok := intValue(raw)
		if !ok || n <= 0 {
			return nil, fmt.Errorf("maxBytes must be a positive integer, got %v", raw)
		}
		max = int64(n)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b bytes.Buffer
	// Read one byte past the limit so oversized files fail without buffering the
	// full payload.
	if _, err := io.CopyN(&b, f, max+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(b.Len()) > max {
		return nil, fmt.Errorf("file %s exceeds maxBytes %d", path, max)
	}
	return parseJSONString(b.String())
}
