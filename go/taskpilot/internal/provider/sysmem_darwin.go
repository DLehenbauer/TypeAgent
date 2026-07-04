package provider

import "golang.org/x/sys/unix"

// probeTotalSystemMemory queries the total physical RAM installed on the
// system, in bytes. The undetermined-memory fallback is applied by
// getTotalSystemMemory, so this returns the raw probe error unchanged.
func probeTotalSystemMemory() (int64, error) {
	// hw.memsize reports total physical memory in bytes.
	memsize, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return int64(memsize), nil
}
