// Package tmpl holds the single path-projection routine shared by the engine and
// the builtin tasks. Centralizing the path projection keeps the key-walking
// semantics identical everywhere so the engine and builtins cannot drift apart.
package tmpl

import "fmt"

// Lookup walks value through path, descending object keys segment by segment. It
// errors when a segment is missing or the current value is not an object. It is
// the single path-projection routine shared by the engine and the builtin JSON
// projection task so their key-walking semantics cannot drift apart.
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
