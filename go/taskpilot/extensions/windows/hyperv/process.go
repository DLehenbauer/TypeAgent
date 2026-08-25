//go:build windows

package hyperv

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is the Windows exit code reported while a process is still running.
const stillActive = 259

// processMatches reports whether pid still identifies the same process
// incarnation that recorded started. A PID alone is not enough: the OS reuses
// PIDs, so a dead owner's PID can name an unrelated live process and make a
// stale lock look held forever. When the start time cannot be read, the caller
// is treated as still owning the lock so a lock is never stolen from a running
// owner.
func processMatches(pid int, started uint64) bool {
	current, alive := processStartToken(pid)
	return alive && (current == 0 || current == started)
}

// processStartToken returns the process creation time as an opaque identity
// token along with whether the process is running. Only ERROR_INVALID_PARAMETER
// proves the ID is unused; every other failure (most commonly
// ERROR_ACCESS_DENIED for a process owned by another account) is reported as
// alive with a zero token, meaning "running, identity unknown".
func processStartToken(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	var code uint32
	// A stale handle can outlive the process it refers to, so the exit code is
	// the real liveness signal.
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return 0, true
	}
	if code != stillActive {
		return 0, false
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, true
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), true
}
