package plan

import (
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/canonical"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/version"
)

// NodeIdentity is the canonical input to NodeID: the task, its version, the
// inputs it consumes, an optional digest of external state, and its
// predecessors. Two nodes share a cache key iff their identities are equal.
type NodeIdentity struct {
	Task    string         `json:"task"`
	Version string         `json:"version"`
	Inputs  map[string]any `json:"inputs"`
	// External is a digest of out-of-IR state the task depends on (e.g. the
	// content hash of a file read). It is omitted for tasks identified by inputs
	// alone, so their cache keys are unaffected.
	External     string   `json:"external,omitempty"`
	Predecessors []string `json:"predecessors"`
}

// NodeID returns a stable cache key derived from identity. The engine
// compatibility version is folded into the hash domain so that crossing a
// compatibility boundary (see version.CompatKey) invalidates all cached node
// outputs. It returns an error only if the identity cannot be canonically
// hashed (e.g. unencodable inputs).
func NodeID(identity NodeIdentity) (string, error) {
	return canonical.Hash("taskpilot.node.v1;engine="+version.CompatKey(), identity)
}
