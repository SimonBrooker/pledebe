package plex

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// Preferences holds the small set of PMS settings pledebe needs.
//
// Preferences.xml also contains PlexOnlineToken and other secrets. This type
// deliberately keeps only whitelisted attributes: everything else is discarded
// during parsing and never stored, logged, or serialised. Do not add a
// catch-all map here.
type Preferences struct {
	// DatabaseBackupPath is where Butler writes dated database backups.
	// Configurable, and NOT necessarily beside the database — verified on a
	// real server 2026-07-28, where it pointed at /backup/Databases while
	// stale backups from an earlier setting sat in the Databases directory.
	//
	// Reading backup freshness from the wrong directory produces a confident,
	// completely wrong "your backups stopped N days ago".
	DatabaseBackupPath string

	// ButlerStartHour and ButlerEndHour bound the window in which PMS runs
	// scheduled tasks. Outside it, nothing runs — so an apparently stalled
	// maintenance job may simply never have been given a window.
	ButlerStartHour string
	ButlerEndHour   string

	// LogDebug reports whether verbose logging is on. Without it, absence of
	// log evidence means nothing.
	LogDebug string
}

// preferenceKeys is the whitelist. Anything not listed here is dropped.
var preferenceKeys = map[string]func(*Preferences, string){
	"ButlerDatabaseBackupPath": func(p *Preferences, v string) { p.DatabaseBackupPath = v },
	"ButlerStartHour":          func(p *Preferences, v string) { p.ButlerStartHour = v },
	"ButlerEndHour":            func(p *Preferences, v string) { p.ButlerEndHour = v },
	"logDebug":                 func(p *Preferences, v string) { p.LogDebug = v },
}

// LoadPreferences reads Preferences.xml from the config root.
//
// The file is a single element carrying every setting as an attribute. A
// missing file is not an error — some installs do not have one yet — and
// callers should treat a zero Preferences as "use defaults".
func (in *Install) LoadPreferences() (*Preferences, error) {
	path := filepath.Join(in.ConfigRoot, "Preferences.xml")
	f, err := os.Open(path)
	if err != nil {
		// A MISSING file means PMS defaults apply, which is fine. An
		// UNREADABLE one means we are blind: we cannot tell where backups go
		// or when Plex runs maintenance, and quietly defaulting produced a
		// confident "backups are 92 days stale" on a server backing up daily.
		if errors.Is(err, fs.ErrNotExist) {
			return &Preferences{}, nil
		}
		if uid, ok := fileOwnerUID(path); ok {
			return &Preferences{}, fmt.Errorf(
				"cannot read %s: it is owned by uid %d and pledebe runs as uid %d — "+
					"set PUID=%d (and PGID to match) so backup and maintenance "+
					"settings can be read",
				path, uid, os.Getuid(), uid)
		}
		return &Preferences{}, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close()

	prefs := &Preferences{}
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err != nil {
			break // EOF or malformed; return whatever we gathered
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, attr := range start.Attr {
			if set, wanted := preferenceKeys[attr.Name.Local]; wanted {
				set(prefs, attr.Value)
			}
		}
	}
	return prefs, nil
}

// BackupDirs returns the directories to search for dated backups, most
// authoritative first.
//
// The configured path is a *container* path from PMS's point of view. pledebe
// sees a different mount namespace, so it may not resolve — in which case we
// still check beside the database, but a caller that finds nothing must not
// conclude backups are missing. Report "cannot see the backup location"
// instead; that is a different message and a much less alarming one.
func (in *Install) BackupDirs(prefs *Preferences) []string {
	var dirs []string
	if in.BackupDirOverride != "" {
		dirs = append(dirs, in.BackupDirOverride)
	}
	if prefs != nil && prefs.DatabaseBackupPath != "" {
		dirs = append(dirs, prefs.DatabaseBackupPath)
	}
	return append(dirs, in.BackupDir)
}

// butlerHours returns Plex's scheduled maintenance window, defaulting to its
// own defaults when the preference is unset. Slow queries during Butler hours
// are expected; the same rate during the evening is not.
func (p *Preferences) butlerHours() (start, end int) {
	start, end = 2, 8 // PMS defaults
	if p == nil {
		return start, end
	}
	if v, err := strconv.Atoi(p.ButlerStartHour); err == nil && v >= 0 && v <= 23 {
		start = v
	}
	if v, err := strconv.Atoi(p.ButlerEndHour); err == nil && v >= 0 && v <= 23 {
		end = v
	}
	return start, end
}
