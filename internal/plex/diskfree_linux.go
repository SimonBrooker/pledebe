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
	// Both operands need an explicit conversion: Bsize is int64 on 64-bit
	// platforms but int32 on 32-bit ones (armv7), and Bavail is unsigned. A
	// bare multiplication compiles on amd64 and fails on arm.
	return int64(st.Bavail) * int64(st.Bsize)
}
