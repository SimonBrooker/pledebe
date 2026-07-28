// Package store persists metric history.
//
// The history is the point: a single reading tells you almost nothing, but
// "the WAL has grown every day for a week" or "this started the day after the
// 1.43.3 upgrade" is actionable. pledebe keeps its own SQLite database in
// /data, entirely separate from Plex's.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the image cross-compiles

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// Store is the metric history database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS samples (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    collected_at       DATETIME NOT NULL,
    database_bytes     INTEGER  NOT NULL,
    wal_bytes          INTEGER  NOT NULL,
    blobs_bytes        INTEGER  NOT NULL,
    page_count         INTEGER  NOT NULL,
    freelist_count     INTEGER  NOT NULL,
    backup_at          DATETIME,
    backup_visible     INTEGER  NOT NULL,
    crash_count        INTEGER  NOT NULL,
    recent_crash_count INTEGER  NOT NULL,
    volume_free_bytes  INTEGER  NOT NULL,
    pms_version        TEXT,
    raw                TEXT     NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_collected_at ON samples(collected_at DESC);
`

// Open creates or opens the history database at path.
func Open(path string) (*Store, error) {
	// _busy_timeout keeps concurrent writes from the scheduler and any future
	// on-demand collection from failing outright.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}
	if _, err := db.Exec(schema + deepCheckSchema + dailySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Insert records one metric sample. The full metrics are stored as JSON so
// fields added later remain queryable in old rows without a migration.
func (s *Store) Insert(m *plex.Metrics) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal metrics: %w", err)
	}

	var backupAt any
	if !m.NewestBackupAt.IsZero() {
		backupAt = m.NewestBackupAt
	}

	_, err = s.db.Exec(`
        INSERT INTO samples (
            collected_at, database_bytes, wal_bytes, blobs_bytes,
            page_count, freelist_count, backup_at, backup_visible,
            crash_count, recent_crash_count, volume_free_bytes,
            pms_version, raw
        ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.CollectedAt, m.DatabaseBytes, m.WALBytes, m.BlobsBytes,
		m.PageCount, m.FreelistCount, backupAt, m.BackupDirVisible,
		m.CrashReportCount, m.RecentCrashCount, m.VolumeFreeBytes,
		m.PMSVersion, string(raw),
	)
	if err != nil {
		return fmt.Errorf("insert sample: %w", err)
	}
	return nil
}

// Latest returns the most recent sample, or nil if there are none.
func (s *Store) Latest() (*plex.Metrics, error) {
	samples, err := s.Recent(1)
	if err != nil || len(samples) == 0 {
		return nil, err
	}
	return samples[0], nil
}

// Recent returns up to limit samples, newest first.
func (s *Store) Recent(limit int) ([]*plex.Metrics, error) {
	rows, err := s.db.Query(
		`SELECT raw FROM samples ORDER BY collected_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query samples: %w", err)
	}
	defer rows.Close()

	var out []*plex.Metrics
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		m := &plex.Metrics{}
		if err := json.Unmarshal([]byte(raw), m); err != nil {
			continue // a malformed row should not sink the whole page
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Count returns how many samples are stored.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM samples`).Scan(&n)
	return n, err
}

// Prune deletes samples older than the retention window. A year of 15-minute
// samples is only ~35k rows, so retention is about tidiness, not space.
func (s *Store) Prune(retain time.Duration) error {
	cutoff := time.Now().Add(-retain)
	_, err := s.db.Exec(`DELETE FROM samples WHERE collected_at < ?`, cutoff)
	return err
}

const deepCheckSchema = `
CREATE TABLE IF NOT EXISTS deep_checks (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at         DATETIME NOT NULL,
    integrity_ok       INTEGER  NOT NULL,
    reclaimable_bytes  INTEGER  NOT NULL,
    error              TEXT,
    raw                TEXT     NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deep_checks_started_at ON deep_checks(started_at DESC);
`

// InsertDeepCheck records one integrity verification.
func (s *Store) InsertDeepCheck(dc *plex.DeepCheck) error {
	raw, err := json.Marshal(dc)
	if err != nil {
		return fmt.Errorf("marshal deep check: %w", err)
	}
	_, err = s.db.Exec(`
        INSERT INTO deep_checks (started_at, integrity_ok, reclaimable_bytes, error, raw)
        VALUES (?,?,?,?,?)`,
		dc.StartedAt, dc.IntegrityOK, dc.ReclaimableBytes, dc.Err, string(raw))
	if err != nil {
		return fmt.Errorf("insert deep check: %w", err)
	}
	return nil
}

// LatestDeepCheck returns the most recent deep check, or nil if none have run.
func (s *Store) LatestDeepCheck() (*plex.DeepCheck, error) {
	var raw string
	err := s.db.QueryRow(
		`SELECT raw FROM deep_checks ORDER BY started_at DESC LIMIT 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query deep check: %w", err)
	}
	dc := &plex.DeepCheck{}
	if err := json.Unmarshal([]byte(raw), dc); err != nil {
		return nil, fmt.Errorf("decode deep check: %w", err)
	}
	return dc, nil
}

// RecentDeepChecks returns up to limit deep checks, newest first.
func (s *Store) RecentDeepChecks(limit int) ([]*plex.DeepCheck, error) {
	rows, err := s.db.Query(
		`SELECT raw FROM deep_checks ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query deep checks: %w", err)
	}
	defer rows.Close()

	var out []*plex.DeepCheck
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		dc := &plex.DeepCheck{}
		if err := json.Unmarshal([]byte(raw), dc); err != nil {
			continue
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}
