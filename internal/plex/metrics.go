package plex

import (
	"context"
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
	NewestBackup     string    `json:"newest_backup,omitempty"`
	NewestBackupAt   time.Time `json:"newest_backup_at,omitempty"`
	BackupCount      int       `json:"backup_count"`
	CrashReportCount int       `json:"crash_report_count"`

	// VolumeFreeBytes is free space where the database lives. Deep checks and
	// repairs need headroom (roughly 1x the database for a snapshot, 3x for a
	// repair), so this gates them.
	VolumeFreeBytes int64 `json:"volume_free_bytes"`
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
func (in *Install) Collect(ctx context.Context, db *SQLite) (*Metrics, error) {
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

	in.collectBackups(m)
	m.CrashReportCount = countEntries(filepath.Join(in.ConfigRoot, "Crash Reports"))
	m.VolumeFreeBytes = freeBytes(filepath.Dir(in.Database))

	return m, nil
}

// collectBackups finds PMS's dated backups, which sit beside the live database
// named like "com.plexapp.plugins.library.db-2026-07-27".
func (in *Install) collectBackups(m *Metrics) {
	entries, err := os.ReadDir(in.BackupDir)
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
