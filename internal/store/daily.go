package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// Tiered retention.
//
// Measured footprint on real-shaped rows: 90 days of 15-minute samples is
// 13.0 MB, while 10 years of daily rows is 7.8 MB. Granularity is what costs,
// not time span -- roughly 0.8 MB per year of daily history.
//
// So fine-grained samples are kept briefly and rolled up into one row per day
// that is never deleted. Deleting at 90 days would discard precisely the signal
// that makes history worth keeping: "the database has grown 40% this year",
// "this started the day after the 1.43.3 upgrade".
const dailySchema = `
CREATE TABLE IF NOT EXISTS daily (
    day               TEXT PRIMARY KEY,
    sample_count      INTEGER NOT NULL,
    database_bytes    INTEGER NOT NULL,
    blobs_bytes       INTEGER NOT NULL,
    wal_bytes_max     INTEGER NOT NULL,
    freelist_count    INTEGER NOT NULL,
    volume_free_min   INTEGER NOT NULL,
    pms_version       TEXT,
    raw               TEXT NOT NULL
);
`

// DailySample is one calendar day of history.
type DailySample struct {
	Day         time.Time `json:"day"`
	SampleCount int       `json:"sample_count"`

	// Point values are taken from the last sample of the day; WAL is the day's
	// maximum and free space its minimum, because for those the extreme is the
	// interesting number, not wherever it happened to sit at midnight.
	DatabaseBytes int64  `json:"database_bytes"`
	BlobsBytes    int64  `json:"blobs_bytes"`
	WALBytesMax   int64  `json:"wal_bytes_max"`
	FreelistCount int64  `json:"freelist_count"`
	VolumeFreeMin int64  `json:"volume_free_bytes_min"`
	PMSVersion    string `json:"pms_version,omitempty"`
}

const dayFormat = "2006-01-02"

// RollupDay aggregates the raw samples for the day containing at, and stores
// the result. Safe to call repeatedly: the day's row is replaced.
//
// Day bounds are computed in Go rather than with SQLite date functions, so the
// stored timestamp format stays an implementation detail of the driver.
func (s *Store) RollupDay(at time.Time) error {
	start := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)

	rows, err := s.db.Query(
		`SELECT raw FROM samples WHERE collected_at >= ? AND collected_at < ?
         ORDER BY collected_at`, start, end)
	if err != nil {
		return fmt.Errorf("query day: %w", err)
	}
	defer rows.Close()

	day := DailySample{Day: start}
	var last *plex.Metrics

	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		m := &plex.Metrics{}
		if err := json.Unmarshal([]byte(raw), m); err != nil {
			continue
		}
		day.SampleCount++
		if m.WALBytes > day.WALBytesMax {
			day.WALBytesMax = m.WALBytes
		}
		if day.VolumeFreeMin == 0 || (m.VolumeFreeBytes > 0 && m.VolumeFreeBytes < day.VolumeFreeMin) {
			day.VolumeFreeMin = m.VolumeFreeBytes
		}
		last = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if last == nil {
		return nil // nothing collected that day
	}

	day.DatabaseBytes = last.DatabaseBytes
	day.BlobsBytes = last.BlobsBytes
	day.FreelistCount = last.FreelistCount
	day.PMSVersion = last.PMSVersion

	raw, err := json.Marshal(last)
	if err != nil {
		return fmt.Errorf("marshal daily: %w", err)
	}

	_, err = s.db.Exec(`
        INSERT INTO daily (day, sample_count, database_bytes, blobs_bytes,
                           wal_bytes_max, freelist_count, volume_free_min,
                           pms_version, raw)
        VALUES (?,?,?,?,?,?,?,?,?)
        ON CONFLICT(day) DO UPDATE SET
            sample_count=excluded.sample_count,
            database_bytes=excluded.database_bytes,
            blobs_bytes=excluded.blobs_bytes,
            wal_bytes_max=excluded.wal_bytes_max,
            freelist_count=excluded.freelist_count,
            volume_free_min=excluded.volume_free_min,
            pms_version=excluded.pms_version,
            raw=excluded.raw`,
		start.Format(dayFormat), day.SampleCount, day.DatabaseBytes, day.BlobsBytes,
		day.WALBytesMax, day.FreelistCount, day.VolumeFreeMin, day.PMSVersion, string(raw))
	if err != nil {
		return fmt.Errorf("upsert daily: %w", err)
	}
	return nil
}

// RecentDays returns up to limit daily rows, newest first.
func (s *Store) RecentDays(limit int) ([]DailySample, error) {
	rows, err := s.db.Query(
		`SELECT day, sample_count, database_bytes, blobs_bytes, wal_bytes_max,
                freelist_count, volume_free_min, pms_version
         FROM daily ORDER BY day DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query daily: %w", err)
	}
	defer rows.Close()

	var out []DailySample
	for rows.Next() {
		var d DailySample
		var dayStr string
		var version *string
		if err := rows.Scan(&dayStr, &d.SampleCount, &d.DatabaseBytes, &d.BlobsBytes,
			&d.WALBytesMax, &d.FreelistCount, &d.VolumeFreeMin, &version); err != nil {
			return nil, err
		}
		d.Day, _ = time.Parse(dayFormat, dayStr)
		if version != nil {
			d.PMSVersion = *version
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DayCount returns how many days of history exist.
func (s *Store) DayCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM daily`).Scan(&n)
	return n, err
}

// PruneRaw deletes fine-grained samples older than retain, rolling each
// affected day up first so nothing is lost. Daily rows are never deleted.
func (s *Store) PruneRaw(retain time.Duration) error {
	cutoff := time.Now().UTC().Add(-retain)

	// Roll up every day that still has raw samples about to be discarded.
	rows, err := s.db.Query(
		`SELECT DISTINCT collected_at FROM samples WHERE collected_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("find expiring samples: %w", err)
	}
	seen := map[string]time.Time{}
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			rows.Close()
			return err
		}
		seen[at.UTC().Format(dayFormat)] = at
	}
	rows.Close()

	for _, at := range seen {
		if err := s.RollupDay(at); err != nil {
			return err
		}
	}

	_, err = s.db.Exec(`DELETE FROM samples WHERE collected_at < ?`, cutoff)
	return err
}
