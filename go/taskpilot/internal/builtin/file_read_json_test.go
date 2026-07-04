package builtin

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTempJSON(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadJSONFileParsesContents(t *testing.T) {
	path := writeTempJSON(t, `{"a":1}`)
	out, err := readJSONFile(context.Background(), map[string]any{"path": path}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": float64(1)}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("out = %#v, want %#v", out, want)
	}
}

func TestReadJSONFileHonorsPositiveMaxBytes(t *testing.T) {
	path := writeTempJSON(t, `{"a":1}`)
	out, err := readJSONFile(context.Background(), map[string]any{"path": path, "maxBytes": 1024}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": float64(1)}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("out = %#v, want %#v", out, want)
	}
}

func TestReadJSONFileEnforcesMaxBytesCap(t *testing.T) {
	path := writeTempJSON(t, `{"a":1}`)
	if _, err := readJSONFile(context.Background(), map[string]any{"path": path, "maxBytes": 2}, Context{}); err == nil {
		t.Fatal("expected error for file exceeding maxBytes, got nil")
	}
}

func TestReadJSONFileRejectsNonPositiveMaxBytes(t *testing.T) {
	path := writeTempJSON(t, `{"a":1}`)
	for _, mb := range []any{0, -1, -100} {
		if _, err := readJSONFile(context.Background(), map[string]any{"path": path, "maxBytes": mb}, Context{}); err == nil {
			t.Fatalf("expected error for maxBytes %v, got nil", mb)
		}
	}
}

func TestReadJSONFileRejectsNonIntegerMaxBytes(t *testing.T) {
	path := writeTempJSON(t, `{"a":1}`)
	if _, err := readJSONFile(context.Background(), map[string]any{"path": path, "maxBytes": "big"}, Context{}); err == nil {
		t.Fatal("expected error for non-integer maxBytes, got nil")
	}
}
