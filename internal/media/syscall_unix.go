//go:build unix

package media

import "syscall"

// syscallStatfs is the statfs(2) shape used for the disk-space check.
type syscallStatfs = syscall.Statfs_t

// statfs fills st for the filesystem holding path.
func statfs(path string, st *syscallStatfs) error { return syscall.Statfs(path, st) }
