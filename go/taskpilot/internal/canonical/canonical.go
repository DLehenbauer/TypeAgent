package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/gowebpki/jcs"
)

// errPrefix labels every error returned by JSON so canonical encoding failures
// share a single wrapping message regardless of which stage produced them.
const errPrefix = "canonical json"

// JSON encodes v into a canonical, deterministic JSON byte slice per RFC 8785
// (JSON Canonicalization Scheme): object keys are sorted, numbers use
// ECMAScript formatting, and no insignificant whitespace is emitted, so equal
// values always produce identical output. It returns an error if v cannot be
// marshaled or transformed into canonical JSON.
func JSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", errPrefix, err)
	}
	out, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", errPrefix, err)
	}
	return out, nil
}

// Hash returns the hex-encoded SHA-256 digest of a canonical JSON object with
// domain and payload fields. The domain separates hashes of structurally
// identical payloads used for different purposes. It returns an error if the
// domain-scoped value cannot be canonically encoded.
func Hash(domain string, payload any) (string, error) {
	raw, err := JSON(map[string]any{"domain": domain, "payload": payload})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
