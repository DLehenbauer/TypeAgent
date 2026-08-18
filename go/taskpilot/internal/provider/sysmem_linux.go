package provider

import "golang.org/x/sys/unix"

// probeTotalSystemMemory reports total physical RAM in bytes from sysinfo.
func probeTotalSystemMemory() (int64, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, err
	}
	// Totalram is expressed in units of info.Unit bytes.
	return int64(info.Totalram) * int64(info.Unit), nil
}
