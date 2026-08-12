//go:build windows

package hyperv

import (
	"os"
	"path/filepath"
	"testing"
)

// A run that is interrupted (Ctrl+C, a killed shell, a crashed process) leaves
// its VM lock file behind. Until the owner's liveness was consulted the next
// run was refused for the whole max-age window, which stranded the developer's
// only test VM for an hour.
func TestHyperVMLockIsTakenOverWhenOwnerProcessIsGone(t *testing.T) {
	stateDir := t.TempDir()

	first, err := acquireHyperVMLock(stateDir, "vm-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !first.Held() {
		t.Fatal("first acquire did not take the lock")
	}

	// Simulate the owner dying without releasing: the file stays, the process
	// does not.
	swapProcessAlive(t, func(int) bool { return false })

	second, err := acquireHyperVMLock(stateDir, "vm-1")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if !second.Held() {
		t.Fatal("lock left by a dead owner was not reclaimed")
	}
}

// The complement of the case above: a lock whose owner is still running must
// never be stolen, or two runs would drive the same VM at once.
func TestHyperVMLockIsRefusedWhileOwnerProcessLives(t *testing.T) {
	stateDir := t.TempDir()

	first, err := acquireHyperVMLock(stateDir, "vm-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !first.Held() {
		t.Fatal("first acquire did not take the lock")
	}

	swapProcessAlive(t, func(int) bool { return true })

	second, err := acquireHyperVMLock(stateDir, "vm-1")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if second.Held() {
		t.Fatal("lock was stolen from a live owner")
	}
}

// A lock file that cannot be parsed carries no owner to check, so it must fall
// back to the age rule instead of being treated as free.
func TestHyperVMLockWithUnreadableOwnerIsNotStolen(t *testing.T) {
	stateDir := t.TempDir()

	if _, err := acquireHyperVMLock(stateDir, "vm-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	path := hyperVMLockPath(stateDir, "vm-1")
	if err := os.WriteFile(path, []byte("not json"), 0o666); err != nil {
		t.Fatalf("corrupt lock: %v", err)
	}

	swapProcessAlive(t, func(int) bool { return false })

	stale, err := hyperVMLockStale(path)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if stale {
		t.Fatal("unparseable lock was reported stale; the age rule should govern instead")
	}
}

// The real liveness probe has to agree with the process table for the obvious
// cases, otherwise the tests above would only be exercising the stub.
func TestProcessAliveMatchesRealProcesses(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive reported the running test binary as dead")
	}
	if processAlive(-1) {
		t.Fatal("processAlive reported an invalid pid as alive")
	}
}

func TestHyperVMLockPathStaysUnderStateDir(t *testing.T) {
	stateDir := t.TempDir()
	path := hyperVMLockPath(stateDir, "vm-1")
	if rel, err := filepath.Rel(stateDir, path); err != nil || rel == "" {
		t.Fatalf("lock path %q escaped state dir %q (err=%v)", path, stateDir, err)
	}
}

func swapProcessAlive(t *testing.T, fn func(int) bool) {
	t.Helper()
	prev := hyperVProcessAlive
	hyperVProcessAlive = fn
	t.Cleanup(func() { hyperVProcessAlive = prev })
}
