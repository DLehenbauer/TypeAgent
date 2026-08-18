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

// fileGlobOutput describes one matched non-directory file. Path is the OS-native
// absolute-or-relative join of Root and the match; RelPath is the match
// relative to Root using forward slashes; BaseName is the file name with its
// extension removed.
type fileGlobOutput struct {
	Path     string `json:"path" required:"true"`
	RelPath  string `json:"relPath" required:"true"`
	BaseName string `json:"baseName" required:"true"`
}

// globInput selects files to match. Root is the directory matching is rooted at;
// Pattern is a doublestar glob (backslashes normalized to forward slashes)
// evaluated relative to Root, supporting "**" for any depth. It is the shared
// input shape for file.glob and file.refGlob, and their content-addressed
// digests, so the schema keys are defined and decoded in exactly one place.
type globInput struct {
	Root    string `json:"root"`
	Pattern string `json:"pattern"`
}

// decodeGlobInput decodes the shared root/pattern selector out of a builtin's
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

// globFiles executes file.glob and returns matched files in the schema shape
// declared by fileGlobOutput.
func globFiles(_ context.Context, input map[string]any, ctx Context) (any, error) {
	in := decodeGlobInput(input)
	if ctx.DryRun {
		// Dry-run returns the empty result shape without walking the filesystem.
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
	// Round-trip the output through the generic JSON encoder so the runtime
	// validates and caches the schema implied by fileGlobOutput.
	return toGenericJSON(out)
}

// relToNative converts a root-relative, slash-separated match into an
// OS-native path under root.
func relToNative(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// globRelPaths returns the sorted, forward-slash relative paths of the
// non-directory files matching pattern under root.
func globRelPaths(root, pattern string) ([]string, error) {
	// doublestar matches against an fs.FS using "/" separators and supports
	// "**" (match across any depth), so the pattern is normalized to forward
	// slashes. Matches come back relative to root.
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	matches, err := doublestar.Glob(os.DirFS(root), pattern)
	if err != nil {
		return nil, err
	}
	// Stabilize traversal order so output and cache digests do not depend on
	// matcher enumeration order.
	sort.Strings(matches)
	out := make([]string, 0, len(matches))
	for _, rel := range matches {
		match := relToNative(root, rel)
		info, err := os.Stat(match)
		if err != nil {
			return nil, err
		}
		// Only files participate in file.glob; directories are filtered out before
		// the result is returned.
		if info.IsDir() {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}
