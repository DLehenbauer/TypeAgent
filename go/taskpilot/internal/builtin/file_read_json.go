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

// FileReadJsonInput configures the file.readJson task. Path is required and
// names the JSON file to read. MaxBytes caps the number of bytes accepted; when
// omitted it defaults to 10 MiB, and when present it must be a positive integer
// (non-positive values are rejected rather than silently coerced). Reads
// exceeding the cap fail with an error. The file contents are parsed as JSON
// and returned.
type FileReadJsonInput struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"maxBytes,omitempty"`
}

var fileReadJSONSpec = model.TaskSpec{
	Name:        "file.readJson",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(FileReadJsonInput{})),
}

// readJSONFile reads the file named by input["path"], enforcing the maxBytes
// cap, and parses the contents as JSON. When maxBytes is omitted it defaults to
// 10 MiB; when present it must be a positive integer, otherwise the read fails.
// Reads exceeding the cap fail. File content that feeds an agent or script
// should flow as a file reference (file.ref) rather than through a whole-file
// read.
func readJSONFile(_ context.Context, input map[string]any, ctx Context) (any, error) {
	if ctx.DryRun {
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
	if _, err := io.CopyN(&b, f, max+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(b.Len()) > max {
		return nil, fmt.Errorf("file %s exceeds maxBytes %d", path, max)
	}
	return parseJSONString(b.String())
}
