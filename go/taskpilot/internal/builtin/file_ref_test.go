package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

func TestFileRefEnvelopeAndFingerprint(t *testing.T) {
	file := filepath.Join(t.TempDir(), "src.txt")
	if err := os.WriteFile(file, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := makeFileRef(nil, pathInput(file), Context{})
	if err != nil {
		t.Fatal(err)
	}
	path, fp, ok := model.AsFileRef(out)
	if !ok {
		t.Fatalf("output is not a file reference: %v", out)
	}
	if path != file {
		t.Fatalf("ref path = %q, want %q", path, file)
	}
	if fp == "" || fp == absentFingerprint {
		t.Fatalf("ref fingerprint = %q, want a content hash", fp)
	}

	// Editing the file changes the fingerprint (content-addressed).
	if err := os.WriteFile(file, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	out2, err := makeFileRef(nil, pathInput(file), Context{})
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, _ := model.AsFileRef(out2)
	if fp2 == fp {
		t.Fatalf("fingerprint did not change after edit: %q", fp2)
	}
}

func TestFileRefMissingFileIsAbsentSentinel(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.txt")
	out, err := makeFileRef(nil, pathInput(missing), Context{})
	if err != nil {
		t.Fatal(err)
	}
	if _, fp, ok := model.AsFileRef(out); !ok || fp != absentFingerprint {
		t.Fatalf("missing-file ref = %v, want absent sentinel", out)
	}
}

func TestFileRefDryRun(t *testing.T) {
	out, err := makeFileRef(nil, pathInput("x.txt"), Context{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, fp, ok := model.AsFileRef(out); !ok || fp != "dry-run" {
		t.Fatalf("dry-run ref = %v, want dry-run fingerprint", out)
	}
}

func TestFileRefGlobProducesReferences(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := makeFileRefGlob(nil, map[string]any{"root": root, "pattern": "*.go"}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	if len(items) != 2 {
		t.Fatalf("refGlob returned %d refs, want 2", len(items))
	}
	for _, it := range items {
		if _, fp, ok := model.AsFileRef(it); !ok || fp == "" {
			t.Fatalf("refGlob item is not a fingerprinted reference: %v", it)
		}
	}
}

func TestGlobContentDigestTracksContent(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.go")
	if err := os.WriteFile(file, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"root": root, "pattern": "*.go"}
	d1, err := globContentDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	// Editing a matched file's content must change the digest even though the
	// match set is unchanged (unlike globDigest, which hashes only paths).
	if err := os.WriteFile(file, []byte("package y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, err := globContentDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatalf("content digest unchanged after edit: %q", d2)
	}
}
