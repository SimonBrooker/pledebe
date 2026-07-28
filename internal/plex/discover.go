// Package plex locates and reads a Plex Media Server installation.
//
// Everything here is read-only. Nothing in this package writes to the Plex
// config directory or to the live database.
package plex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DatabaseName is the marker we scan for. Container images disagree wildly on
// layout — hotio puts the config root at /config with no intermediate dirs,
// lsio and plexinc nest it under "Library/Application Support/Plex Media
// Server", binhex differs again — so we never join known path fragments. We
// look for this file and derive everything else from where we found it.
const DatabaseName = "com.plexapp.plugins.library.db"

// maxScanDepth bounds the walk. The marker sits three levels below the config
// root in every layout seen so far; 6 leaves generous headroom without letting
// a misconfigured mount send us through a media library.
const maxScanDepth = 6

// Install describes a discovered Plex installation.
type Install struct {
	// ConfigRoot is the "Plex Media Server" directory, whatever it is called
	// on this platform. Derived from the database location, not assumed.
	ConfigRoot string

	// Database is the full path to com.plexapp.plugins.library.db.
	Database string

	// BlobsDatabase is the companion blobs database. May be empty if absent.
	BlobsDatabase string

	// LogDir holds "Plex Media Server.log" and its rotations. May be empty.
	LogDir string

	// BackupDir is where PMS writes its dated database backups — the same
	// directory as the database itself.
	BackupDir string
}

// ErrNotFound indicates no Plex database was located under the given roots.
var ErrNotFound = errors.New("no Plex database found")

// Discover scans the given roots for a Plex database and returns the first
// installation found. Roots are searched in order, so pass the most likely
// first. Symlinks are not followed: on a NAS they frequently point into media
// shares, and a scan that wanders into one can take minutes.
func Discover(roots ...string) (*Install, error) {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		if dbPath, err := scan(root); err == nil {
			return newInstall(dbPath)
		}
	}
	return nil, fmt.Errorf("%w under %s", ErrNotFound, strings.Join(roots, ", "))
}

// scan walks root looking for the database marker, bounded by maxScanDepth.
func scan(root string) (string, error) {
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	var found string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable directories are common on NAS appdata shares. Skip
			// rather than abandoning the whole scan.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
			if depth >= maxScanDepth {
				return fs.SkipDir
			}
			return nil
		}

		if d.Name() == DatabaseName {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", ErrNotFound
	}
	return found, nil
}

// newInstall derives the surrounding layout from the database path.
//
// The database always lives at:
//
//	<ConfigRoot>/Plug-in Support/Databases/com.plexapp.plugins.library.db
//
// so the config root is three levels up. Verified against hotio (flat /config)
// and the lsio/plexinc nested layout.
func newInstall(dbPath string) (*Install, error) {
	dbDir := filepath.Dir(dbPath)
	configRoot := filepath.Dir(filepath.Dir(dbDir))

	in := &Install{
		ConfigRoot: configRoot,
		Database:   dbPath,
		BackupDir:  dbDir,
	}

	blobs := filepath.Join(dbDir, "com.plexapp.plugins.library.blobs.db")
	if _, err := os.Stat(blobs); err == nil {
		in.BlobsDatabase = blobs
	}

	logDir := filepath.Join(configRoot, "Logs")
	if info, err := os.Stat(logDir); err == nil && info.IsDir() {
		in.LogDir = logDir
	}

	return in, nil
}
