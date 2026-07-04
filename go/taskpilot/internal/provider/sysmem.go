package provider

// getTotalSystemMemory returns the total physical RAM installed on the system,
// in bytes. It returns 0 when the amount cannot be determined, applying the
// undetermined-memory fallback contract in one place so the per-OS probes in
// probeTotalSystemMemory can stay focused on querying the platform.
func getTotalSystemMemory() int64 {
	bytes, err := probeTotalSystemMemory()
	if err != nil {
		return 0
	}
	return bytes
}
