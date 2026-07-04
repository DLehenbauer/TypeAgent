package model

// fileRefKey is the reserved key identifying a file-reference envelope, a value
// of the shape {"$file": {"path": ..., "fingerprint": ...}}. A file reference
// carries a path and a content fingerprint instead of the file's contents, so
// it flows cheaply through the data plane: consumers (the Copilot agent, pwsh
// scripts) read the file on demand while the fingerprint folds into node
// identity to invalidate downstream caches when the file changes. The key is
// reserved -- task inputs and outputs must not use it for unrelated data.
const fileRefKey = "$file"

// fileRefPath and fileRefFingerprint name the fields inside a file-reference
// envelope's inner object.
const (
	fileRefPath        = "path"
	fileRefFingerprint = "fingerprint"
)

// FileRef builds a file-reference envelope from a path and content fingerprint.
func FileRef(path, fingerprint string) map[string]any {
	return map[string]any{fileRefKey: map[string]any{
		fileRefPath:        path,
		fileRefFingerprint: fingerprint,
	}}
}

// AsFileRef reports whether v is a file-reference envelope and, if so, returns
// its path and fingerprint. It matches the exact shape -- a map whose sole key
// is fileRefKey mapping to an object with a string path -- so ordinary task
// data that merely happens to contain a "$file" key is not misread as a
// reference.
func AsFileRef(v any) (path, fingerprint string, ok bool) {
	m, isMap := v.(map[string]any)
	if !isMap || len(m) != 1 {
		return "", "", false
	}
	inner, isMap := m[fileRefKey].(map[string]any)
	if !isMap {
		return "", "", false
	}
	p, isStr := inner[fileRefPath].(string)
	if !isStr {
		return "", "", false
	}
	fp, _ := inner[fileRefFingerprint].(string)
	return p, fp, true
}

// FileRefPath collapses a file-reference envelope to its on-disk path,
// discarding the identity-only fingerprint. It is the shared coercion for
// consumers that render a reference as a plain path -- the Copilot provider,
// the pwsh args builder, and template expansion -- so they all agree on what a
// resolved reference expands to. It reports false for non-references.
func FileRefPath(v any) (string, bool) {
	p, _, ok := AsFileRef(v)
	return p, ok
}
