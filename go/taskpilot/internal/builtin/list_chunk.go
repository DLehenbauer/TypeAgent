package builtin

import (
	"context"
	"fmt"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// ListChunkInput is the input contract for the list.chunk task. List holds the
// elements to partition, and Size sets the maximum length of each chunk; the
// input schema constrains it to a positive integer (minimum 1). Chunks preserve
// order, and the final chunk may be shorter when len(List) is not a multiple of
// Size.
type ListChunkInput struct {
	List []any `json:"list"`
	Size int   `json:"size"`
}

var listChunkSpec = model.TaskSpec{
	Name:        "list.chunk",
	Version:     "1",
	InputSchema: withMinimum(structToSchema(reflect.TypeOf(ListChunkInput{})), "size", 1),
}

func chunkList(_ context.Context, input map[string]any, _ Context) (any, error) {
	list := asSlice(input["list"])
	size, ok := intValue(input["size"])
	if !ok || size < 1 {
		return nil, fmt.Errorf("list.chunk: size must be a representable positive integer")
	}
	chunks := make([]any, 0)
	for i := 0; i < len(list); i += size {
		end := i + size
		if end > len(list) {
			end = len(list)
		}
		chunks = append(chunks, list[i:end])
	}
	return chunks, nil
}
