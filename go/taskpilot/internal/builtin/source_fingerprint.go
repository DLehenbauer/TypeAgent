package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// FingerprintInput selects the source to hash. Path is required and may name a
// single file or a directory; directories are fingerprinted recursively.
type FingerprintInput struct {
	Path string `json:"path"`
}

// FingerprintOutput reports the SHA-256 fingerprint of the source at Path.
// Fingerprint is the hex-encoded digest; Path echoes the input path.
type FingerprintOutput struct {
	Fingerprint string `json:"fingerprint" required:"true"`
	Path        string `json:"path" required:"true"`
}

var sourceFingerprintSpec = model.TaskSpec{
	Name:        "source.fingerprint",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(FingerprintInput{})),
}

func fingerprintSource(_ context.Context, input map[string]any, ctx Context) (any, error) {
	path := asString(input["path"])
	if ctx.DryRun {
		// FingerprintOutput is the single source of the output field names (its
		// json tags drive the field names); round-trip it into the generic JSON
		// shape the engine validates and caches.
		return toGenericJSON(FingerprintOutput{Path: path, Fingerprint: "dry-run"})
	}
	fp, err := fingerprint(path)
	if err != nil {
		return nil, err
	}
	return toGenericJSON(FingerprintOutput{Path: path, Fingerprint: fp})
}

// fingerprint hashes the source at path. The digester registered for
// source.fingerprint reuses this same function so the cache identity and the
// observable Fingerprint output derive from a single traversal, keeping their
// semantics (file vs. recursive directory) from drifting apart.
func fingerprint(path string) (string, error) {
	h := sha256.New()
	hashFile := func(path, name string) error {
		fmt.Fprintf(h, "file:%s\n", filepath.ToSlash(name))
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(h, f)
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		if err := hashFile(path, filepath.Base(path)); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	var files []string
	if err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, p)
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, p := range files {
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return "", err
		}
		if err := hashFile(p, rel); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
