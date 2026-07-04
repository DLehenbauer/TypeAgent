package provider

import (
	"fmt"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// replaceFileRefs walks v, replacing every file-reference envelope
// ({"$file": {"path": ...}}) with its path string and collecting the referenced
// paths in first-seen order (de-duplicated). The Copilot provider uses it to
// render references as plain paths the agent reads on demand, keeping file
// contents -- and the identity-only fingerprint -- out of the prompt tokens.
func replaceFileRefs(v any) (any, []string) {
	var paths []string
	seen := map[string]bool{}
	var walk func(any) any
	walk = func(cur any) any {
		if p, ok := model.FileRefPath(cur); ok {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
			return p
		}
		switch t := cur.(type) {
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, val := range t {
				out[k] = walk(val)
			}
			return out
		case []any:
			out := make([]any, len(t))
			for i, val := range t {
				out[i] = walk(val)
			}
			return out
		default:
			return cur
		}
	}
	return walk(v), paths
}

// pwshArgs converts a pwsh.run args value into the string arguments passed to
// the shell. The value must be a JSON array (a scalar is rejected rather than
// silently wrapped into a single argument). A file reference collapses to its
// path so the script receives a clean path to stream (Get-Content) rather than
// an encoded envelope; any other item is stringified. The reference fingerprint
// still folds into node identity via the raw input, so the script's result
// re-caches when the file changes.
func pwshArgs(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("pwsh: %s must be an array, got %T", PwshInputArgs, v)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if p, ok := model.FileRefPath(item); ok {
			out = append(out, p)
			continue
		}
		out = append(out, asString(item))
	}
	return out, nil
}
