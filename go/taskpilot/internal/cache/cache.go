// Package cache provides a filesystem-backed store for memoized node outputs,
// including atomic commits, stale-tolerant inflight claims, and garbage
// collection of abandoned staging and claim files.
package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Store is a filesystem-backed cache rooted at a single directory. It is safe
// for concurrent use across processes; commits and claims rely on atomic
// filesystem operations rather than in-process locks.
type Store struct {
	root string
}

// DefaultClaimMaxAge is the age after which an inflight claim is considered
// stale and may be reclaimed by another runner.
const DefaultClaimMaxAge = time.Hour

const (
	stagingDir  = "staging"
	inflightDir = "inflight"
)

// Entry is a cached node output keyed by NodeID, along with the metadata used
// to identify the producing task version and its predecessors. Output holds the
// node's result as a raw JSON document: typing it as json.RawMessage makes
// non-serializable values (channels, funcs, cyclic graphs) unrepresentable at
// this boundary, so an Entry always round-trips through the on-disk format.
type Entry struct {
	NodeID       string          `json:"nodeId"`
	Task         string          `json:"task"`
	Version      string          `json:"version"`
	Predecessors []string        `json:"predecessors"`
	CreatedAt    time.Time       `json:"createdAt"`
	Output       json.RawMessage `json:"output"`
}

// EntryRef is a reference to a committed entry: its node ID and the
// store-relative path to its entry.json file.
type EntryRef struct {
	NodeID string `json:"nodeId"`
	Path   string `json:"path"`
}

type claimInfo struct {
	RunID     string `json:"runId"`
	StartedAt string `json:"startedAt"`
}

// Claim is a handle to the outcome of a claim attempt. Held reports whether the
// caller acquired the inflight claim; when it did, Release relinquishes it. A
// zero Claim (not held) is valid and its Release is a no-op, so callers can
// unconditionally defer Release without branching.
type Claim struct {
	path string
	info claimInfo
	held bool
}

// Held reports whether this caller acquired the claim.
func (c Claim) Held() bool { return c.held }

// Release relinquishes the claim, removing the claim file only if it is still
// owned by this claim's runID. It is a no-op when the claim was not held or has
// already been reclaimed by another runner. It returns an error only on
// unexpected I/O failures.
func (c Claim) Release() error {
	if !c.held {
		return nil
	}
	return releaseClaim(c.path, c.info)
}

// ResolveStateDir returns the directory used for taskpilot state, creating it if
// necessary. It honors TASKPILOT_STATE_DIR, then LOCALAPPDATA, and finally falls
// back to ~/.taskpilot. It returns an error if the directory cannot be created
// or the home directory cannot be determined.
func ResolveStateDir() (string, error) {
	if v := os.Getenv("TASKPILOT_STATE_DIR"); v != "" {
		return v, os.MkdirAll(v, 0o777)
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".taskpilot")
	} else {
		base = filepath.Join(base, "taskpilot")
	}
	return base, os.MkdirAll(base, 0o777)
}

// CacheDir returns the cache subdirectory within the given state directory. It
// is the single source of truth for the cache location so writes and garbage
// collection always operate on the same directory.
func CacheDir(state string) string {
	return filepath.Join(state, "cache")
}

// New returns a Store rooted at the given directory. The directory is not
// created until Init, Commit, or Claim is called.
func New(root string) *Store { return &Store{root: root} }

// Init creates the store's directory layout, including entries, staging,
// inflight, pins, plans, and sources. It is idempotent and returns an error if
// any directory cannot be created.
func (s *Store) Init() error {
	for _, p := range []string{"entries", stagingDir, inflightDir, filepath.Join("pins", "live"), "plans", "sources"} {
		if err := os.MkdirAll(filepath.Join(s.root, p), 0o777); err != nil {
			return err
		}
	}
	return nil
}

// Read returns the cached entry for nodeID. The bool is false when no entry
// exists. A corrupt entry is treated as a miss (returns false) and removed so a
// later Commit can rewrite it; an error is returned only for unexpected I/O
// failures.
func (s *Store) Read(nodeID string) (Entry, bool, error) {
	path := filepath.Join(s.entryDir(nodeID), "entry.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		// A corrupt entry must never be fatal. The cache is an optimization, so an
		// unparseable entry.json (e.g. a partially overwritten file left behind by
		// a historical concurrent-commit collision) is treated as a miss. Discard
		// the poisoned entry so the next Commit rewrites it cleanly instead of
		// failing every future run that touches this node.
		_ = os.RemoveAll(s.entryDir(nodeID))
		return Entry{}, false, nil
	}
	return e, true, nil
}

func (s *Store) entryPath(nodeID string) string {
	return filepath.Join(s.entryDir(nodeID), "entry.json")
}

// EntryRef returns a reference to nodeID's entry, including its store-relative
// path, without checking whether the entry exists.
func (s *Store) EntryRef(nodeID string) EntryRef {
	return EntryRef{
		NodeID: nodeID,
		Path:   filepath.Join("cache", "entries", shardFor(nodeID), nodeID, "entry.json"),
	}
}

// Commit atomically writes e to the cache. If an entry for e.NodeID already
// exists it is a no-op, so commits are idempotent and safe under concurrent
// writers. It returns an error only on unexpected I/O failures.
func (s *Store) Commit(e Entry) error {
	if err := s.Init(); err != nil {
		return err
	}
	target := s.entryDir(e.NodeID)
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	// Stage into a uniquely named directory. A timestamp-based name collides when
	// commits land in the same clock tick (common on Windows, whose clock is
	// coarse), letting concurrent commits clobber each other's entry.json and
	// steal each other's staging dir before the rename. MkdirTemp guarantees a
	// unique directory per commit.
	stageRoot := s.stagingRoot()
	if err := os.MkdirAll(stageRoot, 0o777); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(stageRoot, "commit-")
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "entry.json"), raw, 0o666); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		return err
	}
	if err := os.Rename(stage, target); err != nil {
		if _, statErr := os.Stat(target); statErr == nil {
			_ = os.RemoveAll(stage)
			return nil
		}
		return err
	}
	return nil
}

// Claim attempts to acquire the inflight claim for nodeID on behalf of runID.
// The returned Claim's Held reports whether the claim was acquired; on success
// call Release to relinquish it, which removes the claim only if it is still
// owned by this runID. If the claim is already held by a live runner the
// returned Claim is not held (Held reports false). Stale claims older than
// DefaultClaimMaxAge are reclaimed automatically. An error is returned only on
// unexpected I/O failures.
func (s *Store) Claim(nodeID, runID string) (Claim, error) {
	if err := os.MkdirAll(s.inflightRoot(), 0o777); err != nil {
		return Claim{}, err
	}
	path := s.claimPath(nodeID)
	for {
		info := claimInfo{RunID: runID, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if errors.Is(err, os.ErrExist) {
			removed, err := removeStaleClaim(path)
			if err != nil {
				return Claim{}, err
			}
			if removed {
				continue
			}
			return Claim{}, nil
		}
		if err != nil {
			return Claim{}, err
		}
		if err := json.NewEncoder(f).Encode(info); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return Claim{}, err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return Claim{}, err
		}
		return Claim{path: path, info: info, held: true}, nil
	}
}

// GC removes staging and inflight files whose modification time is older than
// maxAge. It returns the number of removed entries and the first error
// encountered while reading a directory; individual removal failures are
// ignored.
func (s *Store) GC(maxAge time.Duration) (int, error) {
	removed := 0
	for _, dir := range []string{s.stagingRoot(), s.inflightRoot()} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > maxAge {
				if err := os.RemoveAll(path); err == nil {
					removed++
				}
			}
		}
	}
	return removed, nil
}

func removeStaleClaim(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if time.Since(info.ModTime()) <= DefaultClaimMaxAge {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func releaseClaim(path string, claim claimInfo) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var current claimInfo
	if err := json.Unmarshal(raw, &current); err != nil {
		// The claim file is corrupt or was rewritten by another runner; it is not
		// verifiably ours, so leave it in place for stale-claim GC to reclaim.
		return nil
	}
	if current != claim {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) entryDir(nodeID string) string {
	return filepath.Join(s.root, "entries", shardFor(nodeID), nodeID)
}

func (s *Store) stagingRoot() string {
	return filepath.Join(s.root, stagingDir)
}

func (s *Store) inflightRoot() string {
	return filepath.Join(s.root, inflightDir)
}

func (s *Store) claimPath(nodeID string) string {
	return filepath.Join(s.inflightRoot(), nodeID+".claim")
}

func shardFor(nodeID string) string {
	if len(nodeID) > 2 {
		return nodeID[:2]
	}
	return nodeID
}
