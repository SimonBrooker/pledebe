// Package health turns metrics into findings.
//
// This package is deliberately conservative. Seven apparent faults were
// investigated on a healthy server during development and all seven were
// measurement errors or benign quirks (see docs/signals.md). The dominant risk
// is not missing a real problem — it is confidently announcing one that does
// not exist.
//
// Two rules follow:
//
//   - Nothing here derives a finding from FTS internals. Every such signal was
//     rejected during calibration.
//   - When we cannot measure something, we say Unknown. We never infer failure
//     from absence of data.
package health

import (
	"fmt"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// Level ranks a finding.
type Level string

const (
	// LevelOK means measured and healthy.
	LevelOK Level = "ok"
	// LevelUnknown means we could not measure it. Not a fault.
	LevelUnknown Level = "unknown"
	// LevelWarn means measured, and outside expectations.
	LevelWarn Level = "warn"
)

// Finding is one evaluated signal.
type Finding struct {
	Level  Level
	Title  string
	Detail string
}

// backupStaleAfter is generous: Plex's default schedule is every three days,
// and a server that is busy through its Butler window can legitimately skip
// one. Alerting at four days would produce noise on healthy servers.
const backupStaleAfter = 8 * 24 * time.Hour

// walLarge flags a write-ahead log that is not being checkpointed. A healthy
// WAL is tens of MB; sustained hundreds of MB suggests a stuck reader.
const walLarge = 512 << 20

// Evaluate produces findings for a metric sample, most important first.
func Evaluate(m *plex.Metrics) []Finding {
	var warn, unknown, ok []Finding

	add := func(f Finding) {
		switch f.Level {
		case LevelWarn:
			warn = append(warn, f)
		case LevelUnknown:
			unknown = append(unknown, f)
		default:
			ok = append(ok, f)
		}
	}

	add(backupFinding(m))
	add(diskFinding(m))
	add(crashFinding(m))
	add(walFinding(m))
	add(bloatFinding(m))

	return append(append(warn, unknown...), ok...)
}

func backupFinding(m *plex.Metrics) Finding {
	// PMS records its backup path in its own mount namespace. If we cannot see
	// it we know nothing -- reporting "no backups" here was a real false alarm
	// during development, on a server backing up perfectly.
	if m.BackupCount == 0 && !m.BackupDirVisible {
		detail := "pledebe cannot see the backup directory, so freshness is unmeasured"
		if m.BackupDirExpected != "" {
			detail = fmt.Sprintf("PMS writes backups to %s, which is not mounted here", m.BackupDirExpected)
		}
		return Finding{LevelUnknown, "Backup freshness unknown", detail}
	}

	if m.BackupCount == 0 {
		return Finding{LevelWarn, "No database backups found",
			fmt.Sprintf("nothing matching a dated backup in %s", m.BackupDirSearched)}
	}

	age := time.Since(m.NewestBackupAt)
	if age > backupStaleAfter {
		return Finding{LevelWarn, "Database backups are stale",
			fmt.Sprintf("newest is %s, %d days old", m.NewestBackup, int(age.Hours()/24))}
	}

	return Finding{LevelOK, "Database backups current",
		fmt.Sprintf("%d backups, newest %d days old", m.BackupCount, int(age.Hours()/24))}
}

func diskFinding(m *plex.Metrics) Finding {
	if m.VolumeFreeBytes == 0 {
		return Finding{LevelUnknown, "Free space unknown", "could not read the filesystem"}
	}

	// A repair needs roughly 3x the database size. Running out partway through
	// is the worst outcome available, so this gates deep operations.
	switch {
	case m.VolumeFreeBytes < m.DatabaseBytes:
		return Finding{LevelWarn, "Not enough free space for a snapshot",
			fmt.Sprintf("%s free, database is %s", humanBytes(m.VolumeFreeBytes), humanBytes(m.DatabaseBytes))}
	case m.VolumeFreeBytes < m.DatabaseBytes*3:
		return Finding{LevelWarn, "Not enough free space to repair safely",
			fmt.Sprintf("%s free; a repair needs about %s", humanBytes(m.VolumeFreeBytes), humanBytes(m.DatabaseBytes*3))}
	}

	return Finding{LevelOK, "Free space sufficient",
		fmt.Sprintf("%s available", humanBytes(m.VolumeFreeBytes))}
}

func crashFinding(m *plex.Metrics) Finding {
	if m.RecentCrashCount == 0 {
		return Finding{LevelOK, "No recent crashes", "nothing in the last 14 days"}
	}
	return Finding{LevelWarn, "Recent Plex crashes",
		fmt.Sprintf("%d crash files in the last 14 days", m.RecentCrashCount)}
}

func walFinding(m *plex.Metrics) Finding {
	if m.WALBytes > walLarge {
		return Finding{LevelWarn, "Write-ahead log is large",
			fmt.Sprintf("%s -- it may not be checkpointing", humanBytes(m.WALBytes))}
	}
	return Finding{LevelOK, "Write-ahead log normal", humanBytes(m.WALBytes)}
}

// bloatFinding never warns. The freelist is a floor on reclaimable space and
// under-reported by 14x in testing, so it can only justify running a deep
// check -- never a claim about how much space a user would get back.
func bloatFinding(m *plex.Metrics) Finding {
	if m.FreeRatio() > 0.30 {
		return Finding{LevelUnknown, "Database may be bloated",
			fmt.Sprintf("%.0f%% free pages -- a deep check would measure it exactly", m.FreeRatio()*100)}
	}
	return Finding{LevelOK, "No significant bloat",
		fmt.Sprintf("%.1f%% free pages", m.FreeRatio()*100)}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
