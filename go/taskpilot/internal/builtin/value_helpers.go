package builtin

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/tmpl"
)

// lookup resolves a dotted path using the runtime template traversal rules.
func lookup(value any, path []string) (any, bool) {
	segs := make([]any, len(path))
	for i, seg := range path {
		segs[i] = seg
	}
	v, err := tmpl.Lookup(value, segs)
	return v, err == nil
}

// asString formats runtime values as strings; nil becomes "".
func asString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// asSlice returns nil for nil, []any unchanged, and other values as a one-item slice.
func asSlice(v any) []any {
	if v == nil {
		return nil
	}
	if s, ok := v.([]any); ok {
		return s
	}
	return []any{v}
}

// intValue reports an int for supported inputs that can be represented exactly.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		if n < math.MinInt || n > math.MaxInt {
			return 0, false
		}
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < math.MinInt || i > math.MaxInt {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

// numericValue reports the float64 value for supported numeric inputs.
func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// boolValue reports v when it is a bool; other values return false.
func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

// toGenericJSON converts structured values into generic JSON values.
func toGenericJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	return generic, nil
}

// decodeInput decodes a builtin's generic input map into its typed struct.
func decodeInput[T any](input map[string]any) (T, error) {
	var in T
	raw, err := json.Marshal(input)
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, err
	}
	return in, nil
}

// parseJSONString parses JSON text into the generic runtime value graph.
func parseJSONString(text string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, err
	}
	return value, nil
}

// intSet builds a set from list-like numeric input.
func intSet(v any) map[int]bool {
	out := map[int]bool{}
	for _, item := range asSlice(v) {
		if n, ok := intValue(item); ok {
			out[n] = true
		}
	}
	return out
}
