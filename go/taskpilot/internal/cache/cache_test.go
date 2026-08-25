package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimReclaimsStaleClaim(t *testing.T) {
	store := newTestStore(t)
	path := store.claimPath("node")
	old, err := store.Claim("node", "old")
	if err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-DefaultClaimMaxAge - time.Minute)
	if err := os.Chtimes(filepath.Join(path, old.info.Owner), staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	claim, err := store.Claim("node", "new")
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Held() {
		t.Fatal("stale claim was not reclaimed")
	}
	assertClaimRunID(t, path, "new")

	if err := claim.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("claim still exists after release: %v", err)
	}
}

func TestClaimKeepsFreshClaim(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Claim("node", "first")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Held() {
		t.Fatal("initial claim was not acquired")
	}
	second, err := store.Claim("node", "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.Held() {
		t.Fatal("fresh claim should not be reclaimed")
	}
	if err := second.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertClaimRunID(t, store.claimPath("node"), "first")

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(store.claimPath("node")); !os.IsNotExist(err) {
		t.Fatalf("claim still exists after owner release: %v", err)
	}
}

func TestClaimReleaseDoesNotRemoveReclaimedClaim(t *testing.T) {
	store := newTestStore(t)
	path := store.claimPath("node")
	first, err := store.Claim("node", "first")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Held() {
		t.Fatal("initial claim was not acquired")
	}
	staleTime := time.Now().Add(-DefaultClaimMaxAge - time.Minute)
	if err := os.Chtimes(filepath.Join(path, first.info.Owner), staleTime, staleTime); err != nil {
		t.Fatal(err)
	}
	second, err := store.Claim("node", "second")
	if err != nil {
		t.Fatal(err)
	}

	if !second.Held() {
		t.Fatal("stale claim was not reclaimed")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertClaimRunID(t, path, "second")

	if err := second.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("claim still exists after new owner release: %v", err)
	}
}

func TestClaimTouchPreventsReclaim(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Claim("node", "first")
	if err != nil {
		t.Fatal(err)
	}
	path := store.claimPath("node")
	marker := filepath.Join(path, first.info.Owner)
	// Just short of expiry: the heartbeat is what keeps a claim reclaimable only
	// by its owner, so Touch has to refresh a marker that is nearly stale.
	nearlyStale := time.Now().Add(-DefaultClaimMaxAge + time.Minute)
	if err := os.Chtimes(marker, nearlyStale, nearlyStale); err != nil {
		t.Fatal(err)
	}
	if err := first.Touch(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(nearlyStale) {
		t.Fatalf("Touch did not refresh the marker: mtime = %s", info.ModTime())
	}

	second, err := store.Claim("node", "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.Held() {
		t.Fatal("old claim was stolen from a live owner")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestClaimTouchFailsAfterLeaseExpiry pins the fencing rule that lets stale
// reclamation stay lock-free: once a marker ages out, its owner must not be
// able to revive it, because a reclaimer reading the same mtime is already
// entitled to remove it.
func TestClaimTouchFailsAfterLeaseExpiry(t *testing.T) {
	store := newTestStore(t)
	claim, err := store.Claim("node", "owner")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(store.claimPath("node"), claim.info.Owner)
	expired := time.Now().Add(-DefaultClaimMaxAge - time.Minute)
	if err := os.Chtimes(marker, expired, expired); err != nil {
		t.Fatal(err)
	}
	if err := claim.Touch(); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Touch after expiry = %v, want ErrClaimLost", err)
	}
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(expired) {
		t.Fatal("Touch refreshed a marker whose lease had already expired")
	}
}

func TestSweepPreservesOldLiveClaim(t *testing.T) {
	store := newTestStore(t)
	claim, err := store.Claim("node", "owner")
	if err != nil {
		t.Fatal(err)
	}
	path := store.claimPath("node")
	old := time.Now().Add(-DefaultClaimMaxAge + time.Minute)
	if err := os.Chtimes(filepath.Join(path, claim.info.Owner), old, old); err != nil {
		t.Fatal(err)
	}
	if err := claim.Touch(); err != nil {
		t.Fatal(err)
	}

	removed, err := store.Sweep(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	assertClaimRunID(t, path, "owner")
	if err := claim.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimReleaseDoesNotRemoveReplacementAtRemovalBoundary(t *testing.T) {
	store := newTestStore(t)
	path := store.claimPath("node")
	first, err := store.Claim("node", "first")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Held() {
		t.Fatal("initial claim was not acquired")
	}

	var second Claim
	var claimErr error
	err = releaseClaimWithOwnerRemoved(path, first.info, func() {
		second, claimErr = store.Claim("node", "second")
	})
	if err != nil {
		t.Fatalf("release first claim: %v", err)
	}
	if claimErr != nil {
		t.Fatalf("acquire replacement claim: %v", claimErr)
	}
	if !second.Held() {
		t.Fatal("replacement claim was not acquired")
	}
	assertClaimRunID(t, path, "second")

	if err := second.Release(); err != nil {
		t.Fatalf("release replacement claim: %v", err)
	}
}

func TestReadCorruptEntryIsMissAndSelfHeals(t *testing.T) {
	store := newTestStore(t)
	dir := filepath.Join(store.root, entryRelPath("33node"))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	// A valid object followed by a foreign tail, as produced by a partial
	// in-place overwrite. json.Unmarshal rejects this with "invalid character
	// 't' after top-level value".
	corrupt := []byte(`{"nodeId":"33node","task":"file.ref","version":"1","output":"x"}` + "\n" + `true}`)
	if err := os.WriteFile(filepath.Join(dir, "entry.json"), corrupt, 0o666); err != nil {
		t.Fatal(err)
	}

	entry, ok, err := store.Read("33node")
	if err != nil {
		t.Fatalf("Read returned error for corrupt entry: %v", err)
	}
	if ok {
		t.Fatalf("corrupt entry reported as hit: %+v", entry)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("corrupt entry dir not discarded: %v", statErr)
	}

	// A subsequent Commit must repopulate the node cleanly.
	want := Entry{NodeID: "33node", Task: "file.read", Version: "1", Output: json.RawMessage(`"clean"`)}
	if err := store.Commit(want); err != nil {
		t.Fatalf("Commit after self-heal: %v", err)
	}
	got, ok, err := store.Read("33node")
	if err != nil || !ok {
		t.Fatalf("Read after re-commit: ok=%v err=%v", ok, err)
	}
	if string(got.Output) != `"clean"` {
		t.Fatalf("Output = %s, want \"clean\"", got.Output)
	}
}

func TestEntryRef(t *testing.T) {
	store := newTestStore(t)
	ref := store.EntryRef("abcdef")
	if ref.NodeID != "abcdef" {
		t.Fatalf("NodeID = %q", ref.NodeID)
	}
	want := filepath.Join("cache", entryRelPath("abcdef", "entry.json"))
	if ref.Path != want {
		t.Fatalf("Path = %q, want %q", ref.Path, want)
	}
	wantAbs := filepath.Join(store.root, entryRelPath("abcdef", "entry.json"))
	if got := store.entryPath("abcdef"); got != wantAbs {
		t.Fatalf("entryPath = %q, want %q", got, wantAbs)
	}
}

// newTestStore returns an initialized cache store rooted in a per-test temp
// directory, so setup changes touch one bootstrap site instead of every test.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store := New(filepath.Join(t.TempDir(), "cache"))
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	return store
}

// entryRelPath builds the cache-relative entry path for a node, deriving the
// shard from the same shardFor logic the store uses so tests assert one layout.
func entryRelPath(nodeID string, parts ...string) string {
	return filepath.Join(append([]string{"entries", shardFor(nodeID), nodeID}, parts...)...)
}

func assertClaimRunID(t *testing.T, path, want string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("claim owner entries = %d, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(path, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var claim claimInfo
	if err := json.Unmarshal(raw, &claim); err != nil {
		t.Fatal(err)
	}
	if claim.RunID != want {
		t.Fatalf("claim runID = %q, want %q", claim.RunID, want)
	}
}

// Once the owner marker is gone the claim is relinquished, so leftover litter
// must not be reported as a failure: the engine calls Release after the node's
// output is already committed, and an error there would discard a successful
// node and abort the run.
func TestClaimReleaseIgnoresLeftoverClaimDirectory(t *testing.T) {
	store := newTestStore(t)
	path := store.claimPath("node")
	claim, err := store.Claim("node", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Held() {
		t.Fatal("claim was not acquired")
	}

	// Stand in for a directory that survives the owner's removal, which on
	// Windows also happens transiently when a just-unlinked child is still
	// delete-pending.
	err = releaseClaimWithOwnerRemoved(path, claim.info, func() {
		if writeErr := os.WriteFile(filepath.Join(path, "leftover"), []byte("x"), 0o666); writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	if err != nil {
		t.Fatalf("release reported a cleanup failure: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("claim directory should have been left alone: %v", statErr)
	}

	// The leftover is self-healing: the next claimant reclaims it by age.
	staleTime := time.Now().Add(-DefaultClaimMaxAge - time.Minute)
	if err := os.Chtimes(filepath.Join(path, "leftover"), staleTime, staleTime); err != nil {
		t.Fatal(err)
	}
	next, err := store.Claim("node", "next")
	if err != nil {
		t.Fatal(err)
	}
	if !next.Held() {
		t.Fatal("leftover claim directory was not reclaimed")
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}
