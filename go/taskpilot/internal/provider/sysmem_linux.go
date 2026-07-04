package provider

import "golang.org/x/sys/unix"

// probeTotalSystemMemory queries the total physical RAM installed on the
// system, in bytes. The undetermined-memory fallback is applied by
// getTotalSystemMemory, so this returns the raw probe error unchanged.
func probeTotalSystemMemory() (int64, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, err
	}
	// Totalram is expressed in units of info.Unit bytes.
	return int64(info.Totalram) * int64(info.Unit), nil
}
