package model

// fileRefKey is the reserved key identifying a file-reference envelope.
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
// its path and fingerprint.
func AsFileRef(v any) (path, fingerprint string, ok bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", "", false
	}
	inner, ok := m[fileRefKey].(map[string]any)
	if !ok {
		return "", "", false
	}
	p, ok := inner[fileRefPath].(string)
	if !ok {
		return "", "", false
	}
	fp, _ := inner[fileRefFingerprint].(string)
	return p, fp, true
}

// FileRefPath collapses a file-reference envelope to its on-disk path,
// discarding the identity-only fingerprint. It reports false for values that
// are not file references.
func FileRefPath(v any) (string, bool) {
	p, _, ok := AsFileRef(v)
	return p, ok
}
