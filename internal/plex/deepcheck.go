package plex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeepCheck is one integrity verification, run against a snapshot rather than
// the live database.
//
// The technique, validated on a 1139MB library: VACUUM INTO produces a
// transactionally consistent copy in ~4s while PMS keeps running, holding only
// a read lock. The copy can then be checked at leisure, and the size difference
// between original and copy is the EXACT reclaimable space — not the estimate
// the freelist gives, which under-reported by 14x on the same database.
type DeepCheck struct {
	StartedAt   time.Time     `json:"started_at"`
	Duration    time.Duration `json:"duration"`
	SnapshotSec float64       `json:"snapshot_seconds"`
	CheckSec    float64       `json:"check_seconds"`

	DatabaseBytes    int64 `json:"database_bytes"`
	SnapshotBytes    int64 `json:"snapshot_bytes"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`

	// IntegrityOK is PRAGMA integrity_check returning "ok". Note this is the
	// main database check, which is trustworthy. FTS integrity-check is NOT run
	// here: it reports corruption on healthy databases (see docs/signals.md).
	IntegrityOK     bool   `json:"integrity_ok"`
	IntegrityDetail string `json:"integrity_detail,omitempty"`

	Err string `json:"error,omitempty"`
}

// deepCheckTimeout is generous: a snapshot of a very large library on slow
// spinning disks can take many minutes.
const deepCheckTimeout = 45 * time.Minute

// snapshotHeadroom is how much free space we insist on relative to the
// database size before starting. The snapshot is roughly the size of the
// database; running the volume out of space mid-write is the worst outcome
// available to a monitoring tool.
const snapshotHeadroom = 1.2

// RunDeepCheck snapshots the database into scratchDir, verifies the snapshot,
// and removes it.
//
// The live database is never written to. If anything fails, the snapshot is
// still cleaned up.
func (in *Install) RunDeepCheck(ctx context.Context, db *SQLite, scratchDir string) (*DeepCheck, error) {
	dc := &DeepCheck{StartedAt: time.Now().UTC()}
	defer func() { dc.Duration = time.Since(dc.StartedAt) }()

	dc.DatabaseBytes = fileSize(in.Database)
	if dc.DatabaseBytes == 0 {
		dc.Err = "could not stat the database"
		return dc, fmt.Errorf("%s", dc.Err)
	}

	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		dc.Err = "scratch directory unavailable: " + err.Error()
		return dc, err
	}

	// Gate on free space before doing anything expensive.
	free := freeBytes(scratchDir)
	needed := int64(float64(dc.DatabaseBytes) * snapshotHeadroom)
	if free > 0 && free < needed {
		dc.Err = fmt.Sprintf("need ~%d MB free in scratch, have %d MB",
			needed/(1<<20), free/(1<<20))
		return dc, fmt.Errorf("%s", dc.Err)
	}

	ctx, cancel := context.WithTimeout(ctx, deepCheckTimeout)
	defer cancel()

	// A fresh name each run: a leftover file from a previous crash must never
	// be mistaken for this run's snapshot.
	snapshot := filepath.Join(scratchDir,
		fmt.Sprintf("snapshot-%d.db", dc.StartedAt.UnixNano()))
	defer os.Remove(snapshot)

	// The default query timeout is far too short for this.
	deep := &SQLite{BinaryPath: db.BinaryPath, Timeout: deepCheckTimeout}

	t0 := time.Now()
	if _, err := deep.Query(ctx, in.Database,
		fmt.Sprintf("VACUUM INTO '%s';", escapeSQLiteString(snapshot))); err != nil {
		dc.Err = "snapshot failed: " + err.Error()
		return dc, err
	}
	dc.SnapshotSec = time.Since(t0).Seconds()

	dc.SnapshotBytes = fileSize(snapshot)
	if dc.SnapshotBytes > 0 {
		// Exact, not estimated. This is the number a user should be shown.
		dc.ReclaimableBytes = dc.DatabaseBytes - dc.SnapshotBytes
	}

	t1 := time.Now()
	out, err := deep.Query(ctx, snapshot, "PRAGMA integrity_check;")
	dc.CheckSec = time.Since(t1).Seconds()
	if err != nil {
		dc.Err = "integrity check failed to run: " + err.Error()
		return dc, err
	}

	first := strings.TrimSpace(firstLine(out))
	dc.IntegrityOK = first == "ok"
	if !dc.IntegrityOK {
		// Keep it bounded: integrity_check can return a great many lines.
		dc.IntegrityDetail = truncate(out, 2000)
	}

	return dc, nil
}

// escapeSQLiteString doubles single quotes for use inside a SQL string
// literal. Scratch paths are operator-controlled, but a path containing a
// quote would otherwise produce a syntax error at best.
func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… truncated"
}
