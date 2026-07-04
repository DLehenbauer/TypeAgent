package builtin

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// fileGlobOutput is one matched non-directory file. Path is the OS-native
// absolute-or-relative join of Root and the match; RelPath is the match
// relative to Root using forward slashes; BaseName is the file name with its
// extension removed.
type fileGlobOutput struct {
	Path     string `json:"path" required:"true"`
	RelPath  string `json:"relPath" required:"true"`
	BaseName string `json:"baseName" required:"true"`
}

// globInput selects files to match. Root is the directory matching is rooted
// at; Pattern is a doublestar glob (backslashes normalized to forward slashes)
// evaluated relative to Root, supporting "**" for any depth. It is the shared
// input shape for file.glob and file.refGlob (and their content-addressed
// digests), so the schema keys are defined and decoded in exactly one place.
type globInput struct {
	Root    string `json:"root"`
	Pattern string `json:"pattern"`
}

// decodeGlobInput reads the shared root/pattern selector out of a builtin's
// generic input map.
func decodeGlobInput(input map[string]any) globInput {
	return globInput{
		Root:    asString(input["root"]),
		Pattern: asString(input["pattern"]),
	}
}

func fileGlobSpec() model.TaskSpec {
	return model.TaskSpec{
		Name:        "file.glob",
		Version:     "1",
		InputSchema: structToSchema(reflect.TypeOf(globInput{})),
	}
}

func globFiles(_ context.Context, input map[string]any, ctx Context) (any, error) {
	in := decodeGlobInput(input)
	if ctx.DryRun {
		return []any{}, nil
	}
	rels, err := globRelPaths(in.Root, in.Pattern)
	if err != nil {
		return nil, err
	}
	out := make([]fileGlobOutput, 0, len(rels))
	for _, rel := range rels {
		match := relToNative(in.Root, rel)
		baseName := strings.TrimSuffix(filepath.Base(match), filepath.Ext(match))
		out = append(out, fileGlobOutput{Path: match, RelPath: rel, BaseName: baseName})
	}
	// fileGlobOutput is the single source of the output field names (its json
	// tags drive the field names); round-trip it into the generic JSON shape
	// the engine validates and caches.
	return toGenericJSON(out)
}

// relToNative joins a root-relative, forward-slash path (as produced by
// globRelPaths) with root into an OS-native filesystem path. It is the single
// encoding shared by every glob/ref flow that turns a match's relative path
// back into a path the filesystem can open.
func relToNative(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// globRelPaths returns the sorted, forward-slash relative paths of the non-dir
// files matching pattern under root. Shared by file.glob execution and its
// content-addressed cache digest so both observe an identical match set.
func globRelPaths(root, pattern string) ([]string, error) {
	// doublestar matches against an fs.FS using "/" separators and supports
	// "**" (match across any depth), so the pattern is normalized to forward
	// slashes. Matches come back relative to root.
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	matches, err := doublestar.Glob(os.DirFS(root), pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	out := make([]string, 0, len(matches))
	for _, rel := range matches {
		match := relToNative(root, rel)
		info, err := os.Stat(match)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}
