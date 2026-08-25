package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
)

func TestChunkListPartitionsInOrder(t *testing.T) {
	out, err := chunkList(context.Background(), map[string]any{
		"list": []any{1, 2, 3, 4, 5},
		"size": 2,
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{[]any{1, 2}, []any{3, 4}, []any{5}}
	if got := out.([]any); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks = %v, want %v", got, want)
	}
}

func TestChunkListEmptySerializesAsArray(t *testing.T) {
	out, err := chunkList(context.Background(), map[string]any{
		"list": []any{},
		"size": 1,
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "[]" {
		t.Fatalf("empty chunks JSON = %s, want []", got)
	}
}

func TestChunkListRejectsUnrepresentableSize(t *testing.T) {
	_, err := chunkList(context.Background(), map[string]any{
		"list": []any{1},
		"size": json.Number("1e100"),
	}, Context{})
	if err == nil {
		t.Fatal("expected an error for an unrepresentable chunk size")
	}
}

func TestListChunkSchemaEncodesSizeMinimum(t *testing.T) {
	props := listChunkSpec.InputSchema.(map[string]any)["properties"].(map[string]any)
	size := props["size"].(map[string]any)
	if _, ok := size["minimum"]; !ok {
		t.Fatalf("size schema missing minimum: %v", size)
	}
	if err := schema.Validate(listChunkSpec.InputSchema, map[string]any{
		"list": []any{1, 2},
		"size": 0,
	}); err == nil {
		t.Fatal("expected schema validation to reject size 0, got nil")
	}
	if err := schema.Validate(listChunkSpec.InputSchema, map[string]any{
		"list": []any{1, 2},
		"size": 1,
	}); err != nil {
		t.Fatalf("size 1 should validate: %v", err)
	}
}
