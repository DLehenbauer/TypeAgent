package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"reflect"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// FileRefInput selects the file to reference. Path is required and names the
// file whose reference is produced.
type FileRefInput struct {
	Path string `json:"path"`
}

var fileRefSpec = model.TaskSpec{
	Name:        "file.ref",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(FileRefInput{})),
}

// makeFileRef produces a file-reference envelope for input["path"]. The
// non-dry-run fingerprint is the file content digest, with absentFingerprint
// used for a missing file.
func makeFileRef(_ context.Context, input map[string]any, ctx Context) (any, error) {
	path := asString(input["path"])
	if ctx.DryRun {
		// Dry runs return a stable placeholder without inspecting the filesystem.
		return model.FileRef(path, "dry-run"), nil
	}
	fp, err := hashFileContent(path)
	if err != nil {
		return nil, err
	}
	return model.FileRef(path, fp), nil
}

var fileRefGlobSpec = model.TaskSpec{
	Name:        "file.refGlob",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(globInput{})),
}

// makeFileRefGlob produces file-reference envelopes for every non-directory
// file matching pattern under root, sorted by relative path.
func makeFileRefGlob(_ context.Context, input map[string]any, ctx Context) (any, error) {
	in := decodeGlobInput(input)
	if ctx.DryRun {
		// Dry-run returns the empty result shape without walking the filesystem.
		return []any{}, nil
	}
	rels, err := globRelPaths(in.Root, in.Pattern)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rels))
	for _, rel := range rels {
		match := relToNative(in.Root, rel)
		fp, err := hashFileContent(match)
		if err != nil {
			return nil, err
		}
		out = append(out, model.FileRef(match, fp))
	}
	return out, nil
}

// globContentDigest hashes the sorted file.refGlob match set and each match's
// content fingerprint so path and content changes invalidate identity.
func globContentDigest(input map[string]any) (string, error) {
	in := decodeGlobInput(input)
	rels, err := globRelPaths(in.Root, in.Pattern)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, rel := range rels {
		io.WriteString(h, rel)
		h.Write([]byte{0})
		fp, err := hashFileContent(relToNative(in.Root, rel))
		if err != nil {
			return "", err
		}
		io.WriteString(h, fp)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
