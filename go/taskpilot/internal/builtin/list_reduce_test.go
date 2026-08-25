package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func TestReduceListSumNumeric(t *testing.T) {
	out, err := reduceList(context.Background(), map[string]any{
		"list": []any{1, 2, 3},
		"op":   "sum",
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.(float64), 6.0; got != want {
		t.Fatalf("sum = %v, want %v", got, want)
	}
}

func TestReduceListSumRejectsNonNumericItem(t *testing.T) {
	_, err := reduceList(context.Background(), map[string]any{
		"list": []any{1, "nope", 3},
		"op":   "sum",
	}, Context{})
	if err == nil {
		t.Fatal("expected error for non-numeric item, got nil")
	}
}

func TestReduceListSumRejectsNonNumericAccumulator(t *testing.T) {
	_, err := reduceList(context.Background(), map[string]any{
		"list":    []any{1, 2},
		"op":      "sum",
		"initial": "seed",
	}, Context{})
	if err == nil {
		t.Fatal("expected error for non-numeric accumulator, got nil")
	}
}

func TestReduceListAppendAlwaysSerializesAsArray(t *testing.T) {
	for _, op := range []string{"", "append"} {
		t.Run(op, func(t *testing.T) {
			for _, list := range [][]any{{}, {1}} {
				out, err := reduceList(context.Background(), map[string]any{
					"list": list,
					"op":   op,
				}, Context{})
				if err != nil {
					t.Fatal(err)
				}
				got, err := json.Marshal(out)
				if err != nil {
					t.Fatal(err)
				}
				want := "[]"
				if len(list) == 1 {
					want = "[1]"
				}
				if string(got) != want {
					t.Fatalf("reduce JSON = %s, want %s", got, want)
				}
			}
		})
	}
}
