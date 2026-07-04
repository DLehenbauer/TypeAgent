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

// makeFileRef produces a file-reference envelope for path. The fingerprint is
// the SHA-256 of the file's content (an absent file yields the
// absentFingerprint sentinel), so the reference -- and any downstream node it
// feeds -- changes
// when the file's content changes. Consumers read the file on demand rather
// than receiving its bytes inline.
func makeFileRef(_ context.Context, input map[string]any, ctx Context) (any, error) {
	path := asString(input["path"])
	if ctx.DryRun {
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

// makeFileRefGlob produces a file-reference envelope for every non-directory
// file matching pattern under root, sorted by relative path. It is the batch
// form of file.ref: a single node yields references for a whole match set,
// which downstream nodes can fan out over.
func makeFileRefGlob(_ context.Context, input map[string]any, ctx Context) (any, error) {
	in := decodeGlobInput(input)
	if ctx.DryRun {
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

// globContentDigest is the external-state digester for file.refGlob. Unlike
// globDigest (which hashes only the match set, since file.glob reports paths),
// file.refGlob embeds each file's content fingerprint in its output, so its
// identity must also fold in content: the node re-runs when any matched file's
// content changes, not only when files are added or removed.
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
