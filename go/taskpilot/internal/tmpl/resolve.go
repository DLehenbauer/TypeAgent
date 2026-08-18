// Package tmpl contains path-projection helpers used by builtins.
package tmpl

import "fmt"

// Lookup walks value through a path, descending object keys segment by segment.
// It errors when a segment is missing or the current value is not an object; the
// segment is converted to a string before the map lookup.
func Lookup(value any, path []any) (any, error) {
	for _, seg := range path {
		obj, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("path segment %q not found", seg)
		}
		v, exists := obj[fmt.Sprint(seg)]
		if !exists {
			return nil, fmt.Errorf("path segment %q not found", seg)
		}
		value = v
	}
	return value, nil
}
