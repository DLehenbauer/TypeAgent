package builtin

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/tmpl"
)

func lookup(value any, path []string) (any, bool) {
	segs := make([]any, len(path))
	for i, seg := range path {
		segs[i] = seg
	}
	v, err := tmpl.Lookup(value, segs)
	return v, err == nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func asSlice(v any) []any {
	if v == nil {
		return nil
	}
	if s, ok := v.([]any); ok {
		return s
	}
	return []any{v}
}

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

// numericValue reports the float64 value of v when it is a supported numeric
// type. The bool result is false for any non-numeric value (including json.Number
// strings that fail to parse), letting callers enforce numeric invariants instead
// of silently coercing to zero.
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

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

// toGenericJSON marshals v and unmarshals it back into the generic JSON value
// shape (map[string]any, []any, ...) that the engine validates and caches. It
// lets builtins source their output shape from a typed struct while still
// emitting the plain JSON values the runtime expects.
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

// decodeInput coerces the generic map[string]any input the runtime hands each
// builtin into the task's typed input struct. It round-trips through JSON so
// numeric widening, field naming, and missing-field defaults follow a single
// set of encoding/json rules; every builtin that needs typed input decodes
// through here so that behavior cannot drift between tasks.
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

// parseJSONString decodes JSON text into the generic JSON value shape
// (map[string]any, []any, ...). It is the single decoding path shared by
// json.parse and file.readJson so their behavior and errors stay in sync.
func parseJSONString(text string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, err
	}
	return value, nil
}

func intSet(v any) map[int]bool {
	out := map[int]bool{}
	for _, item := range asSlice(v) {
		if n, ok := intValue(item); ok {
			out[n] = true
		}
	}
	return out
}
