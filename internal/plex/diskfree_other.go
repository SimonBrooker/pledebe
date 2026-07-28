//go:build !linux

package plex

// freeBytes is a no-op off Linux. pledebe ships as a Linux container; this
// exists only so the package builds on a developer machine (Windows/macOS).
func freeBytes(string) int64 { return 0 }
