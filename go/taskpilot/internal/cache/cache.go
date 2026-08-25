// Package cache provides a filesystem-backed store for memoized node outputs,
// including atomic commits, stale-tolerant inflight claims, and garbage
// collection of abandoned staging and claim files.
package cache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store is a filesystem-backed cache rooted at a single directory. Commits and
// claim acquisition rely on atomic filesystem operations rather than in-process
// locks.
type Store struct {
	root string
}

// DefaultClaimMaxAge is the age after which an inflight claim is considered
// stale and may be reclaimed by another runner.
const DefaultClaimMaxAge = time.Hour

var ErrClaimLost = errors.New("cache claim ownership lost")

const (
	stagingDir  = "staging"
	inflightDir = "inflight"
	// claimSuffix names the per-node claim directory inside inflightDir.
	claimSuffix = ".claim"
	// claimStagePrefix names the scratch directory a claim attempt renames into
	// place; leftovers from an interrupted attempt are swept by age.
	claimStagePrefix = ".claim-stage-"
	// claimOwnerPrefix names the marker file inside a claim directory. The
	// suffix is random so no later attempt can produce the same name, which is
	// what lets an owner remove its own marker without ever removing another's.
	claimOwnerPrefix = "owner-"
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

// claimInfo records the runner that owns an inflight claim and when it started.
// Owner names the marker file that represents this owner inside the claim
// directory; it is what makes an owner's mutations affect only its own claim.
type claimInfo struct {
	RunID     string `json:"runId"`
	StartedAt string `json:"startedAt"`
	Owner     string `json:"owner"`
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

// Release relinquishes the claim by removing this owner's marker, after
// checking that the marker on disk is still ours. It is a no-op when the claim
// was not held or has already been reclaimed by another runner. Leftover
// directory litter is not an error: the next Claim or Sweep reclaims it. An
// error means the marker itself could not be unlinked, so the claim stays until
// it ages out.
func (c Claim) Release() error {
	if !c.held {
		return nil
	}
	return releaseClaim(c.path, c.info)
}

// Touch refreshes a held claim after verifying its owner marker.
//
// A marker that has already aged past DefaultClaimMaxAge is forfeit: Touch
// reports ErrClaimLost instead of reviving it. Reclamation decides staleness
// from the same mtime this check reads, so an owner can never resurrect a lease
// a reclaimer has already judged stale and is about to remove. That symmetry is
// what makes stale reclamation safe without a cross-process lock: the owner
// fences itself at exactly the instant a competitor becomes entitled to take
// over.
func (c Claim) Touch() error {
	if !c.held {
		return nil
	}
	ownerPath := filepath.Join(c.path, c.info.Owner)
	stat, err := os.Stat(ownerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrClaimLost, c.path)
		}
		return err
	}
	if age := time.Since(stat.ModTime()); age > DefaultClaimMaxAge {
		return fmt.Errorf("%w: %q went %s without a heartbeat", ErrClaimLost, c.path, age.Round(time.Second))
	}
	raw, err := os.ReadFile(ownerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrClaimLost, c.path)
		}
		return err
	}
	var current claimInfo
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	if current != c.info {
		return fmt.Errorf("%w: %q is no longer owned by run %q", ErrClaimLost, c.path, c.info.RunID)
	}
	now := time.Now()
	if err := os.Chtimes(ownerPath, now, now); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrClaimLost, c.path)
		}
		return err
	}
	return nil
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
// call Release to relinquish it. If an unexpired claim already exists, the
// returned Claim is not held (Held reports false). Stale claims older than
// DefaultClaimMaxAge are reclaimed automatically. An error is returned only on
// unexpected I/O failures.
func (s *Store) Claim(nodeID, runID string) (Claim, error) {
	root := s.inflightRoot()
	if err := os.MkdirAll(root, 0o777); err != nil {
		return Claim{}, err
	}
	owner, err := newClaimOwner()
	if err != nil {
		return Claim{}, err
	}
	info := claimInfo{
		RunID:     runID,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Owner:     owner,
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return Claim{}, err
	}
	stage, err := os.MkdirTemp(root, claimStagePrefix)
	if err != nil {
		return Claim{}, err
	}
	held := false
	defer func() {
		if !held {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.WriteFile(filepath.Join(stage, owner), append(raw, '\n'), 0o666); err != nil {
		return Claim{}, err
	}
	path := s.claimPath(nodeID)
	retriedMissingTarget := false
	for {
		// Installing a populated directory makes acquisition atomic on Windows
		// and Unix without relying on rename-overwrite behavior.
		if err := os.Rename(stage, path); err == nil {
			held = true
			return Claim{path: path, info: info, held: true}, nil
		} else if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			// The previous owner may have removed the target after Rename
			// observed it. Retry once with the still-intact staging directory.
			if !retriedMissingTarget {
				retriedMissingTarget = true
				continue
			}
			return Claim{}, err
		}
		retriedMissingTarget = false
		removed, err := removeStaleClaim(path)
		if err != nil {
			return Claim{}, err
		}
		if removed {
			continue
		}
		return Claim{}, nil
	}
}

// newClaimOwner returns a marker name that no other claim attempt can produce.
// Uniqueness is what allows an owner to remove its own marker without ever
// removing a replacement owner's, including after a stale sweep removed it.
func newClaimOwner() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return claimOwnerPrefix + hex.EncodeToString(buf[:]), nil
}

// Sweep removes staging and inflight files whose modification time is older
// than maxAge. It returns the number of removed entries and the first error
// encountered while reading a directory; individual removal failures are
// ignored.
//
// This reclaims by age rather than by reachability, which is why it is not
// called GC: nothing here traces which entries are still referenced. It is the
// store's half of what `tp cache gc` does, alongside target.Registry.SweepAll
// for external target state.
func (s *Store) Sweep(maxAge time.Duration) (int, error) {
	removed := 0
	staging, err := os.ReadDir(s.stagingRoot())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	removed += sweepAged(s.stagingRoot(), staging, maxAge)

	inflight, err := os.ReadDir(s.inflightRoot())
	if errors.Is(err, os.ErrNotExist) {
		return removed, nil
	}
	if err != nil {
		return removed, err
	}
	// Claims carry per-owner liveness, so they are aged marker by marker.
	// Everything else in the directory is staging scratch abandoned by an
	// interrupted claim attempt, which is aged as a whole.
	var abandoned []os.DirEntry
	for _, e := range inflight {
		if !strings.HasSuffix(e.Name(), claimSuffix) {
			abandoned = append(abandoned, e)
			continue
		}
		ok, err := removeStaleClaimAfter(filepath.Join(s.inflightRoot(), e.Name()), maxAge)
		if err != nil {
			return removed, err
		}
		if ok {
			removed++
		}
	}
	return removed + sweepAged(s.inflightRoot(), abandoned, maxAge), nil
}

// sweepAged removes the entries of dir whose mtime is older than maxAge and
// reports how many were removed.
func sweepAged(dir string, entries []os.DirEntry, maxAge time.Duration) int {
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed
}

// removeStaleClaim reports whether claim acquisition can be retried. Claims are
// directories containing an owner-specific marker, so stale cleanup removes
// only the owners it observed and removes the claim directory only while empty.
func removeStaleClaim(path string) (bool, error) {
	return removeStaleClaimAfter(path, DefaultClaimMaxAge)
}

func removeStaleClaimAfter(path string, maxAge time.Duration) (bool, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if time.Since(info.ModTime()) <= maxAge {
			return false, nil
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(path, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return removeEmptyClaimDir(path)
}

// releaseClaim best-effort releases a claim that matches the recorded owner.
func releaseClaim(path string, claim claimInfo) error {
	return releaseClaimWithOwnerRemoved(path, claim, nil)
}

// releaseClaimWithOwnerRemoved exposes the point between removing the owner's
// unique marker and conditionally removing the now-empty claim directory.
// ownerRemoved is a test seam: it is the only way to deterministically drive a
// replacement owner into that window, which is the race the marker protocol
// exists to survive. Production callers pass nil via releaseClaim.
func releaseClaimWithOwnerRemoved(path string, claim claimInfo, ownerRemoved func()) error {
	if claim.Owner == "" {
		return fmt.Errorf("cache claim %q has no owner marker", path)
	}
	ownerPath := filepath.Join(path, claim.Owner)
	raw, err := os.ReadFile(ownerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var current claimInfo
	if err := json.Unmarshal(raw, &current); err != nil {
		// The claim file is corrupt or was rewritten by another runner; it is not
		// verifiably ours, so leave it in place for the stale-claim sweep to
		// reclaim.
		return nil
	}
	if current != claim {
		return nil
	}
	if err := os.Remove(ownerPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if ownerRemoved != nil {
		ownerRemoved()
	}
	// Ownership ends with the marker. The now-empty directory is only litter,
	// and on Windows rmdir routinely fails while another handle still holds a
	// just-unlinked child. Reclaiming it is left to the next Claim or Sweep so a
	// transient cleanup failure cannot fail a node whose work already committed.
	_, _ = removeEmptyClaimDir(path)
	return nil
}

// removeEmptyClaimDir atomically removes path only if it is still empty and
// reports whether it is gone. A replacement owner makes the directory
// non-empty, so this cannot remove a claim installed after the previous owner's
// marker was removed. This works on Windows, where renaming over an existing
// directory is not supported, as well as on filesystems where a rename may
// replace an empty directory.
func removeEmptyClaimDir(path string) (bool, error) {
	if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else {
		entries, readErr := os.ReadDir(path)
		if errors.Is(readErr, os.ErrNotExist) {
			return true, nil
		}
		if readErr == nil && len(entries) != 0 {
			return false, nil
		}
		return false, err
	}
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
	return filepath.Join(s.inflightRoot(), nodeID+claimSuffix)
}

func shardFor(nodeID string) string {
	if len(nodeID) > 2 {
		return nodeID[:2]
	}
	return nodeID
}
