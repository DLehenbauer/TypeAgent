package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintFramesFileNamesAndContents(t *testing.T) {
	first := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "a"), []byte("X"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "b"), []byte("Y"), 0o666); err != nil {
		t.Fatal(err)
	}

	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(second, "a"), []byte("Xfile:b\nY"), 0o666); err != nil {
		t.Fatal(err)
	}

	firstHash, err := fingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := fingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash == secondHash {
		t.Fatalf("structurally different directories produced %q", firstHash)
	}
}
