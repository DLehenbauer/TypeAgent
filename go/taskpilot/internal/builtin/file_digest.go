package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// absentFingerprint is the stable sentinel returned for a missing file, shared
// by every producer and test so the value has a single definition site.
const absentFingerprint = "absent"

// fileContentDigest hashes the content of the file named by input["path"] so a
// content-addressed node re-runs when the file changes and cache-hits when it
// does not. A missing file yields a stable absentFingerprint sentinel rather
// than an error, keeping identity computable; the task's own execution surfaces
// the missing-file error if it matters.
func fileContentDigest(input map[string]any) (string, error) {
	return hashFileContent(asString(input["path"]))
}

func hashFileContent(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return absentFingerprint, nil
		}
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileExistsDigest captures only presence/absence, the sole external state that
// changes file.exists output. Content is irrelevant to the result.
func fileExistsDigest(input map[string]any) (string, error) {
	present, err := filePresent(asString(input["path"]))
	if err != nil {
		return "", err
	}
	if present {
		return "present", nil
	}
	return absentFingerprint, nil
}

// globDigest hashes the sorted set of matched relative paths. Content changes do
// not alter file.glob output (it reports paths, not contents), so they are
// intentionally excluded; downstream file.ref nodes capture content via their
// own digest. The digest invalidates on add/remove/rename of matches.
func globDigest(input map[string]any) (string, error) {
	in := decodeGlobInput(input)
	rels, err := globRelPaths(in.Root, in.Pattern)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, r := range rels {
		io.WriteString(h, r)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
