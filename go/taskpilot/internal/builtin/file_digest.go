package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// absentFingerprint is the stable sentinel used when a file-dependent digest
// references a missing file.
const absentFingerprint = "absent"

// fileContentDigest hashes the content of input["path"]. Missing files produce
// absentFingerprint so cache identity stays deterministic.
func fileContentDigest(input map[string]any) (string, error) {
	return hashFileContent(asString(input["path"]))
}

// hashFileContent returns the SHA-256 content digest for path, or
// absentFingerprint when path does not exist.
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

// fileExistsDigest records only whether input["path"] exists; content changes
// do not affect file.exists.
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

// globDigest hashes the sorted file.glob match set. It intentionally ignores
// file content because file.glob returns paths only.
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
