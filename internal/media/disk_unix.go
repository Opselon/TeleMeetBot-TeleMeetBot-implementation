//go:build unix

package media

import "syscall"

// diskUsageForPath reports the free bytes available to an unprivileged
// caller (statfs bavail) and the total bytes of the filesystem holding path.
func diskUsageForPath(path string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), true
}
