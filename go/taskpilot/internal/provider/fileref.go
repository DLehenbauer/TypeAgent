package provider

import "github.com/microsoft/TypeAgent/go/taskpilot/internal/model"

// replaceFileRefs walks v, replacing every file-reference envelope with its
// path and collecting paths in first-seen order (de-duplicated).
func replaceFileRefs(v any) (any, []string) {
	var paths []string
	seen := map[string]bool{}
	var walk func(any) any
	walk = func(cur any) any {
		if path, ok := model.FileRefPath(cur); ok {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
			return path
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
