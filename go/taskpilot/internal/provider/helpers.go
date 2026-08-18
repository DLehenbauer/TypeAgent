package provider

import (
	"encoding/json"
	"fmt"
)

// asString converts a provider input value to a string.
func asString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// intValue converts numeric-like provider input values to ints when exact.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}
