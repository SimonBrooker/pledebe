//go:build linux

package plex

import (
	"os"
	"syscall"
)

// fileOwnerUID returns the uid owning path, so an unreadable file can be
// reported with the exact PUID needed to fix it rather than a vague
// "permission denied".
func fileOwnerUID(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
