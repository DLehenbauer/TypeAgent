//go:build windows

package hyperv

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is the Windows exit code reported while a process is still running.
const stillActive = 259

// processAlive reports whether a process ID still identifies a running process.
// Only ERROR_INVALID_PARAMETER proves the ID is unused; every other failure
// (most commonly ERROR_ACCESS_DENIED for a process owned by another account) is
// reported as alive so a lock is never stolen from a running owner.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	var code uint32
	// A stale handle can outlive the process it refers to, so the exit code is
	// the real liveness signal.
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return true
	}
	return code == stillActive
}
