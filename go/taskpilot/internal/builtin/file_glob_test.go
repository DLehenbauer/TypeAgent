package builtin

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

func TestFileGlobRecursiveDoubleStar(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"top.go",
		filepath.Join("a", "one.go"),
		filepath.Join("a", "b", "two.go"),
		filepath.Join("a", "b", "note.txt"),
		filepath.Join("a", "b", "c", "three.go"),
	}
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		pattern string
		want    []string
	}{
		{"**/*.go", []string{"a/b/c/three.go", "a/b/two.go", "a/one.go", "top.go"}},
		{"a/**/*.go", []string{"a/b/c/three.go", "a/b/two.go", "a/one.go"}},
		{"**/*.txt", []string{"a/b/note.txt"}},
		{"a/**", []string{"a/b/c/three.go", "a/b/note.txt", "a/b/two.go", "a/one.go"}},
		{"*.go", []string{"top.go"}}, // non-recursive fast path is unaffected
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			out, err := globFiles(nil, map[string]any{"root": root, "pattern": tc.pattern}, Context{})
			if err != nil {
				t.Fatal(err)
			}
			items := out.([]any)
			got := make([]string, 0, len(items))
			for _, it := range items {
				got = append(got, it.(map[string]any)["relPath"].(string))
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Fatalf("pattern %q: got %v, want %v", tc.pattern, got, want)
			}
		})
	}
}

func TestFileGlobDryRunReturnsEmpty(t *testing.T) {
	out, err := globFiles(nil, map[string]any{"root": ".", "pattern": "**/*.go"}, Context{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.([]any); len(got) != 0 {
		t.Fatalf("dry-run glob = %v, want empty", got)
	}
}
