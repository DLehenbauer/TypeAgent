package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func TestFlattenListEmptySerializesAsArray(t *testing.T) {
	out, err := flattenList(context.Background(), map[string]any{
		"list": []any{},
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "[]" {
		t.Fatalf("empty flattened list JSON = %s, want []", got)
	}
}
