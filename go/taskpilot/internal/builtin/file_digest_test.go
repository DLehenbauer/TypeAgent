package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

// pathInput builds the standard single-path input map for file builtins,
// localizing the "path" key schema to one place across the package's tests.
func pathInput(p string) map[string]any {
	return map[string]any{"path": p}
}

func TestFileContentDigestReflectsContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1, err := fileContentDigest(pathInput(p))
	if err != nil {
		t.Fatal(err)
	}

	// Same content -> stable digest.
	d1b, err := fileContentDigest(pathInput(p))
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d1b {
		t.Fatalf("digest not stable: %q vs %q", d1, d1b)
	}

	// Changed content -> different digest.
	if err := os.WriteFile(p, []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, err := fileContentDigest(pathInput(p))
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatalf("digest unchanged after content change: %q", d2)
	}
}

func TestFileContentDigestAbsentIsStable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.txt")
	d, err := fileContentDigest(pathInput(p))
	if err != nil {
		t.Fatal(err)
	}
	if d != absentFingerprint {
		t.Fatalf("missing file digest = %q, want absent", d)
	}
}

func TestFileExistsDigestTracksPresence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if d, _ := fileExistsDigest(pathInput(p)); d != absentFingerprint {
		t.Fatalf("digest = %q, want absent", d)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, _ := fileExistsDigest(pathInput(p)); d != "present" {
		t.Fatalf("digest = %q, want present", d)
	}
}

func TestGlobDigestTracksMembershipNotContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"root": dir, "pattern": "*.go"}
	d1, err := globDigest(in)
	if err != nil {
		t.Fatal(err)
	}

	// Editing a member's content must NOT change the glob digest: file.glob
	// reports paths, and downstream readers capture content via their own digest.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a // changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1b, err := globDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d1b {
		t.Fatalf("glob digest changed on content edit: %q vs %q", d1, d1b)
	}

	// Adding a matching file must change the digest.
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, err := globDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatalf("glob digest unchanged after adding a file: %q", d2)
	}
}
