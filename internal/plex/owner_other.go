//go:build !linux

package plex

// fileOwnerUID is unavailable off Linux. pledebe ships as a Linux container;
// this exists so the package builds on a developer machine.
func fileOwnerUID(string) (int, bool) { return 0, false }
