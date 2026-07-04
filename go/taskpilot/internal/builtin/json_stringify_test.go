package builtin

import (
	"context"
	"testing"
)

func TestJSONStringifyCompactSortsKeys(t *testing.T) {
	out, err := stringifyJSON(context.Background(), map[string]any{
		"value": map[string]any{"b": 2, "a": 1},
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"a":1,"b":2}` {
		t.Fatalf("stringify = %q", out)
	}
}

func TestJSONStringifyIndent(t *testing.T) {
	out, err := stringifyJSON(context.Background(), map[string]any{
		"value":  []any{1, 2},
		"indent": 2,
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	want := "[\n  1,\n  2\n]"
	if out != want {
		t.Fatalf("stringify indent = %q, want %q", out, want)
	}
}
