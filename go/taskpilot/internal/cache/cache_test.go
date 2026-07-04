package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimReclaimsStaleClaim(t *testing.T) {
	store := newTestStore(t)
	path := store.claimPath("node")
	if err := os.WriteFile(path, []byte(`{"runId":"old","startedAt":"old"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-DefaultClaimMaxAge - time.Minute)
	if err := os.Chtimes(path, staleTime, staleTime); err != nil {
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
	if err := os.Chtimes(path, staleTime, staleTime); err != nil {
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
	raw, err := os.ReadFile(path)
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
