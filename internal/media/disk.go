package media

// DiskUsageForPathPublic reports the caller-visible free bytes and the total
// bytes of the volume holding path. ok=false means the lookup failed and
// callers must treat free space as unknown, never as zero.
func DiskUsageForPathPublic(path string) (free, total uint64, ok bool) {
	return diskUsageForPath(path)
}

// diskUsageBytes returns the same numbers for in-package callers.
func diskUsageBytes(path string) (free, total uint64, ok bool) {
	return diskUsageForPath(path)
}
