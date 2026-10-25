//go:build unix

package health

import "syscall"

// diskUsage returns the free and total byte counts of the filesystem holding
// path. Free is the count available to an unprivileged caller (statfs bavail).
func diskUsage(path string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), true
}
