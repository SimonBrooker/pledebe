package plex

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Metrics is one cheap poll of database health. Everything here is a stat call
// or a read-only pragma — no snapshot, no locks held beyond an instant. Safe to
// run every few minutes against a live server.
type Metrics struct {
	CollectedAt time.Time `json:"collected_at"`

	DatabaseBytes int64 `json:"database_bytes"`
	WALBytes      int64 `json:"wal_bytes"`
	SHMBytes      int64 `json:"shm_bytes"`
	BlobsBytes    int64 `json:"blobs_bytes"`

	PageCount     int64 `json:"page_count"`
	PageSize      int64 `json:"page_size"`
	FreelistCount int64 `json:"freelist_count"`

	// FreelistBytes is free pages x page size. This is a FLOOR on reclaimable
	// space, not an estimate of it: it misses intra-page fragmentation. On a
	// healthy 1139MB library it reported 1MB where a snapshot proved 14MB —
	// wrong by 14x, in the direction that hides bloat.
	//
	// Use it to trigger a deep check. Never show it to a user as "reclaimable".
	FreelistBytes int64 `json:"freelist_bytes"`

	// NewestBackup is the most recent PMS-generated dated backup. A stale or
	// missing backup is a real finding: Butler's backup job failing silently is
	// common, and nobody notices until they need it.
	NewestBackup   string    `json:"newest_backup,omitempty"`
	NewestBackupAt time.Time `json:"newest_backup_at,omitempty"`
	BackupCount    int       `json:"backup_count"`

	// BackupDirSearched is where the backups above were found, and
	// BackupDirVisible reports whether the configured backup location is
	// reachable from pledebe's mount namespace at all.
	//
	// PMS's ButlerDatabaseBackupPath is a path inside the PMS container. If we
	// cannot see it, we know nothing about backup freshness and must say so
	// rather than reporting the stale leftovers next to the database.
	BackupDirSearched string `json:"backup_dir_searched,omitempty"`
	BackupDirVisible  bool   `json:"backup_dir_visible"`
	BackupDirExpected string `json:"backup_dir_expected,omitempty"`

	// PreferencesNote records why Preferences.xml could not be read, if it
	// could not. Empty means it was read, or is genuinely absent.
	//
	// Without it pledebe does not know where PMS writes backups or when it runs
	// maintenance, so several findings must report Unknown rather than defaults.
	PreferencesNote string `json:"preferences_note,omitempty"`

	// BackupMountUnused names a directory that looks like a mounted backup
	// location the operator forgot to set PLEX_BACKUP_DIR to. The mount and the
	// variable are configured in different places, so having one without the
	// other is a predictable mistake worth naming precisely.
	BackupMountUnused string `json:"backup_mount_unused,omitempty"`

	// BackupDirAuthoritative reports whether the directory we scanned is the
	// one PMS actually writes to.
	//
	// When false, anything found there is NOT evidence about backup freshness.
	// Dated files linger beside the database long after
	// ButlerDatabaseBackupPath was pointed elsewhere, and reporting their age
	// produced a confident "92 days stale" on a server backing up daily.
	BackupDirAuthoritative bool `json:"backup_dir_authoritative"`

	// CrashReportCount counts crash FILES, not directories.
	//
	// "Crash Reports" contains one directory per PMS version ever installed —
	// 171 of them on a long-lived server going back years. Counting entries
	// reports version history as crashes. Verified 2026-07-28.
	CrashReportCount int       `json:"crash_report_count"`
	RecentCrashCount int       `json:"recent_crash_count"`
	NewestCrashAt    time.Time `json:"newest_crash_at,omitempty"`

	// CrashesByComponent splits crashes by the component that produced them —
	// "PLEX MEDIA SERVER", "PLEX MEDIA SCANNER", "PLEX TUNER SERVICE".
	CrashesByComponent map[string]int `json:"crashes_by_component,omitempty"`

	// LastCrash describes the most recent crash. A count alone is not
	// actionable; what crashed, under which Plex version, and what the log said
	// just before it died, is.
	LastCrash *CrashReport `json:"last_crash,omitempty"`

	// PMSVersion is inferred from the newest "Crash Reports" subdirectory, and
	// VersionSeenAt is when that directory appeared — an approximate upgrade
	// date. Schema migrations are a risk window, so knowing a problem began
	// hours after an upgrade is worth more than most metrics here.
	PMSVersion    string    `json:"pms_version,omitempty"`
	VersionSeenAt time.Time `json:"version_seen_at,omitempty"`

	// SlowQueries summarises PMS's own slow-query warnings since the previous
	// poll. Plex decides what counts as slow, so this is one of the few signals
	// pledebe does not have to invent a threshold for.
	SlowQueries *SlowQueries `json:"slow_queries,omitempty"`

	// VolumeFreeBytes is free space where the database lives. Deep checks and
	// repairs need headroom (roughly 1x the database for a snapshot, 3x for a
	// repair), so this gates them.
	VolumeFreeBytes int64 `json:"volume_free_bytes"`
}

// CrashReport is the most recent crash, with an excerpt where one is readable.
type CrashReport struct {
	At         time.Time `json:"at"`
	Component  string    `json:"component"`
	PMSVersion string    `json:"pms_version"`
	File       string    `json:"file"`

	// Excerpt is the tail of the crash log, redacted. Empty for bare .dmp
	// minidumps, which are binary and meaningless to display.
	Excerpt string `json:"excerpt,omitempty"`
}

// FreeRatio returns free pages as a fraction of total pages.
func (m Metrics) FreeRatio() float64 {
	if m.PageCount == 0 {
		return 0
	}
	return float64(m.FreelistCount) / float64(m.PageCount)
}

// Collect gathers a cheap metric sample.
//
// Errors reading individual optional values are not fatal — a missing blobs
// database or Logs directory is normal on some installs, and a partial sample
// is more useful than none.
//
// since bounds slow-query log scanning to lines newer than the previous poll.
// Pass the zero time on first run to take whatever the logs still hold.
func (in *Install) Collect(ctx context.Context, db *SQLite, since time.Time) (*Metrics, error) {
	m := &Metrics{CollectedAt: time.Now().UTC()}

	m.DatabaseBytes = fileSize(in.Database)
	m.WALBytes = fileSize(in.Database + "-wal")
	m.SHMBytes = fileSize(in.Database + "-shm")
	if in.BlobsDatabase != "" {
		m.BlobsBytes = fileSize(in.BlobsDatabase)
	}

	var err error
	if m.PageCount, err = db.QueryInt(ctx, in.Database, "PRAGMA page_count;"); err != nil {
		return nil, err
	}
	if m.PageSize, err = db.QueryInt(ctx, in.Database, "PRAGMA page_size;"); err != nil {
		return nil, err
	}
	if m.FreelistCount, err = db.QueryInt(ctx, in.Database, "PRAGMA freelist_count;"); err != nil {
		return nil, err
	}
	m.FreelistBytes = m.FreelistCount * m.PageSize

	prefs, prefsErr := in.LoadPreferences()
	if prefsErr != nil {
		m.PreferencesNote = prefsErr.Error()
	}
	in.collectBackups(m, prefs)
	butlerStart, butlerEnd := prefs.butlerHours()
	m.SlowQueries = in.collectSlowQueries(since, butlerStart, butlerEnd)
	in.collectCrashes(m)
	m.VolumeFreeBytes = freeBytes(filepath.Dir(in.Database))

	return m, nil
}

// collectBackups finds PMS's dated backups, named like
// "com.plexapp.plugins.library.db-2026-07-27".
//
// They do NOT reliably live beside the database: ButlerDatabaseBackupPath is
// configurable, and stale backups from a previous setting can sit in the
// database directory long after PMS stopped writing there. Search the
// configured location first.
func (in *Install) collectBackups(m *Metrics, prefs *Preferences) {
	if prefs != nil {
		m.BackupDirExpected = prefs.DatabaseBackupPath
	}

	// A directory is authoritative if the operator pointed us at it, or if PMS
	// says it writes there. The fallback beside the database is not, unless no
	// other path is configured — it may hold leftovers from an earlier setting.
	authoritative := map[string]bool{}
	if in.BackupDirOverride != "" {
		authoritative[in.BackupDirOverride] = true
	}
	m.BackupMountUnused = findUnusedBackupMount(in.BackupDirOverride)

	configured := prefs != nil && prefs.DatabaseBackupPath != ""
	if configured {
		authoritative[prefs.DatabaseBackupPath] = true
	}

	for _, dir := range in.BackupDirs(prefs) {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		m.BackupDirVisible = true
		m.BackupDirSearched = dir
		m.BackupDirAuthoritative = authoritative[dir] || !configured
		in.scanBackupDir(dir, m)
		if m.BackupCount > 0 {
			return
		}
	}
}

func (in *Install) scanBackupDir(dir string, m *Metrics) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := DatabaseName + "-"
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		stamp := strings.TrimPrefix(e.Name(), prefix)
		at, err := time.Parse("2006-01-02", stamp)
		if err != nil {
			continue // not a dated backup; PMS also leaves other suffixes here
		}
		m.BackupCount++
		if at.After(m.NewestBackupAt) {
			m.NewestBackupAt = at
			m.NewestBackup = e.Name()
		}
	}
}

// recentCrashWindow bounds what counts as a "recent" crash. Old crashes are
// history; a cluster in the last fortnight is a signal.
const recentCrashWindow = 14 * 24 * time.Hour

// collectCrashes walks "Crash Reports". Verified layout, 2026-07-28:
//
//	Crash Reports/1.43.3.10828-00f62d37d/PLEX MEDIA SERVER/<crash files>
//	Crash Reports/1.43.3.10828-00f62d37d/PLEX MEDIA SCANNER/<crash files>
//	Crash Reports/1.43.3.10828-00f62d37d/PLEX TUNER SERVICE/<crash files>
//
// Two levels, not one. An earlier version counted top-level entries and
// reported 171 PMS *versions* as 171 crashes; its replacement read only one
// level down and would have reported zero however many crashes existed. Walk
// the whole tree.
//
// The component directory is worth keeping: "the Scanner crashed" and "the
// Server crashed" are different problems.
func (in *Install) collectCrashes(m *Metrics) {
	root := filepath.Join(in.ConfigRoot, "Crash Reports")

	versions, err := os.ReadDir(root)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-recentCrashWindow)
	m.CrashesByComponent = make(map[string]int)
	defer func() {
		// Read the excerpt once, after the walk has settled on a winner —
		// rather than re-reading a 6 MB log every time a newer file is found.
		if m.LastCrash != nil && hasReadableLog(m.LastCrash.File) {
			m.LastCrash.Excerpt = crashExcerpt(filepath.Join(
				root, m.LastCrash.PMSVersion, m.LastCrash.Component, m.LastCrash.File))
		}
	}()

	for _, v := range versions {
		if !v.IsDir() {
			continue
		}
		if info, err := v.Info(); err == nil && info.ModTime().After(m.VersionSeenAt) {
			m.VersionSeenAt = info.ModTime()
			m.PMSVersion = v.Name()
		}

		versionDir := filepath.Join(root, v.Name())
		_ = filepath.WalkDir(versionDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			// The directory also holds .RateLimit.json, the crash uploader's own
			// state. It is rewritten constantly, so counting it would report a
			// fresh crash every day forever.
			if !isCrashFile(d.Name()) {
				return nil
			}

			m.CrashReportCount++

			// Component is the first path element below the version directory.
			component := ""
			if rel, relErr := filepath.Rel(versionDir, path); relErr == nil {
				if parts := strings.Split(rel, string(os.PathSeparator)); len(parts) > 1 {
					component = parts[0]
					m.CrashesByComponent[component]++
				}
			}

			info, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			if info.ModTime().After(m.NewestCrashAt) {
				m.NewestCrashAt = info.ModTime()
				m.LastCrash = &CrashReport{
					At:         info.ModTime(),
					Component:  component,
					PMSVersion: v.Name(),
					File:       d.Name(),
				}
			}
			if info.ModTime().After(cutoff) {
				m.RecentCrashCount++
			}
			return nil
		})
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func countEntries(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// conventionalBackupMounts are the paths the documentation tells operators to
// mount backups at. Probing them lets pledebe distinguish "you have not set
// this up" from "you set it up but forgot the variable" — a predictable
// mistake, since the mount and the variable are configured in different places.
var conventionalBackupMounts = []string{"/plexbackups", "/plexbackup"}

// findUnusedBackupMount reports a directory that looks like a backup mount the
// operator forgot to point PLEX_BACKUP_DIR at.
//
// It only fires when the directory actually contains dated backups: an empty
// mount is far more likely to be an unrelated volume than a misconfiguration,
// and suggesting otherwise would send someone chasing a setting they do not
// need.
func findUnusedBackupMount(override string) string {
	if override != "" {
		return "" // already configured; nothing to suggest
	}
	for _, dir := range conventionalBackupMounts {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if countDatedBackups(dir) > 0 {
			return dir
		}
	}
	return ""
}

// countDatedBackups counts PMS-style dated backups without recording them, so a
// probe cannot be mistaken for a measurement.
func countDatedBackups(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	prefix := DatabaseName + "-"
	var n int
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if _, err := time.Parse("2006-01-02", strings.TrimPrefix(e.Name(), prefix)); err == nil {
			n++
		}
	}
	return n
}
