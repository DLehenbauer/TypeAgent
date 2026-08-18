package provider

import "golang.org/x/sys/unix"

// probeTotalSystemMemory reports total physical RAM in bytes from hw.memsize.
func probeTotalSystemMemory() (int64, error) {
	// hw.memsize reports total physical memory in bytes.
	memsize, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return int64(memsize), nil
}
