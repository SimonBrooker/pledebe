//go:build linux

package plex

import "syscall"

// freeBytes reports free space available to an unprivileged process on the
// filesystem containing path. Returns 0 if it cannot be determined.
func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}
