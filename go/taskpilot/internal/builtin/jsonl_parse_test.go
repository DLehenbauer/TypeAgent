package builtin

import (
	"context"
	"reflect"
	"testing"
)

func TestParseJSONLParsesLines(t *testing.T) {
	out, err := parseJSONL(context.Background(), map[string]any{
		"text": "{\"a\":1}\n\n{\"b\":2}\n",
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"a": float64(1)},
		map[string]any{"b": float64(2)},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("out = %#v, want %#v", out, want)
	}
}

func TestParseJSONLRejectsMissingText(t *testing.T) {
	if _, err := parseJSONL(context.Background(), map[string]any{}, Context{}); err == nil {
		t.Fatal("expected error for missing text, got nil")
	}
}

func TestParseJSONLRejectsNonStringText(t *testing.T) {
	if _, err := parseJSONL(context.Background(), map[string]any{"text": 42}, Context{}); err == nil {
		t.Fatal("expected error for non-string text, got nil")
	}
}
