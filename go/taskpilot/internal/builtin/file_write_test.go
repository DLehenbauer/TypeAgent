package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.txt")
	in := map[string]any{"path": p, "content": "hello"}
	got, err := writeFile(context.Background(), in, Context{})
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("return = %v, want %q", got, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q, want %q", string(b), "hello")
	}
}

func TestWriteFileRejectsEmptyPath(t *testing.T) {
	in := map[string]any{"path": "", "content": "hello"}

	// Real-write path must reject an empty path before touching the disk.
	if _, err := writeFile(context.Background(), in, Context{}); err == nil {
		t.Fatal("expected error for empty path on real write, got nil")
	}

	// Dry-run path must reject it too, rather than reporting a bogus write.
	if _, err := writeFile(context.Background(), in, Context{DryRun: true}); err == nil {
		t.Fatal("expected error for empty path on dry-run, got nil")
	}
}

func TestFileWriteIsNeverMemoized(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.txt")
	rt := RuntimeRegistry()
	input := map[string]any{"path": p, "content": "desired"}

	memoize, digest, err := rt.CacheBehavior(fileWriteSpec.Name, input)
	if err != nil {
		t.Fatal(err)
	}
	if memoize || digest != "" {
		t.Fatalf("file.write cache behavior = (%v, %q), want non-cacheable", memoize, digest)
	}
}
