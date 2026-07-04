package canonical

import "testing"

func TestHashIsCanonical(t *testing.T) {
	a, err := Hash("test", map[string]any{"b": 2, "a": 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash("test", map[string]any{"a": 1, "b": 2})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("expected canonical hashes to match: %s != %s", a, b)
	}
}
