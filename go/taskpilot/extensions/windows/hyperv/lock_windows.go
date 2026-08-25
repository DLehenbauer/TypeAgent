//go:build windows

package hyperv

import (
	"os"

	"golang.org/x/sys/windows"
)

// withHyperVMLockGuard runs fn while holding an exclusive advisory lock for the
// VM lock at path, making read-compare-remove sequences atomic across processes.
//
// The guard is a separate zero-byte file because the lock file itself is what
// the sequence creates and deletes; a lock taken on it could not span its own
// removal. Guard files are intentionally never deleted: one persists per VM name
// and deleting one would let two processes guard different inodes for the same
// lock.
func withHyperVMLockGuard(path string, fn func() error) error {
	guard, err := os.OpenFile(path+".guard", os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return err
	}
	defer guard.Close()

	var overlapped windows.Overlapped
	if err := windows.LockFileEx(
		windows.Handle(guard.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&overlapped,
	); err != nil {
		return err
	}
	defer func() {
		_ = windows.UnlockFileEx(windows.Handle(guard.Fd()), 0, 1, 0, &overlapped)
	}()
	return fn()
}
