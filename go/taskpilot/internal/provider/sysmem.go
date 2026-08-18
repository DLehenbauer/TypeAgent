package provider

// getTotalSystemMemory returns total physical RAM in bytes, or 0 when the
// platform probe cannot determine it.
func getTotalSystemMemory() int64 {
	bytes, err := probeTotalSystemMemory()
	if err != nil {
		return 0
	}
	return bytes
}
