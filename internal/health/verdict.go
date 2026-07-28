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
//   - When we cannot measure something, we say Unknown. We never infer failure
//     from absence of data.
//   - A finding must name the symptom a user would actually notice, not the
//     internal state we measured.
//
// The second rule came from nearly getting FTS wrong. Every calibration test
// was a read — MATCH queries, row counts, UI search — and they all passed, so
// the failing integrity-check looked like a false positive. DBRepair documents
// the real symptom as occurring on WRITES: adding to collections, editing
// metadata. Testing the half that works proves nothing about the half that
// does not.
package health

import (
	"fmt"
	"strings"
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
	// LevelFault means measured, and broken now. Reserved for states that are
	// unambiguously wrong -- not merely worth attention -- so that red keeps
	// its meaning.
	LevelFault Level = "fault"
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

// integrityStaleAfter is how long a deep check stays meaningful. Corruption
// does not appear from nowhere, but a check from last month says little about
// today.
const integrityStaleAfter = 48 * time.Hour

// Evaluate produces findings for a metric sample, most important first.
//
// dc is the most recent deep check and may be nil — no integrity verification
// has run yet, which is Unknown, not a fault.
func Evaluate(m *plex.Metrics, dc *plex.DeepCheck) []Finding {
	var fault, warn, unknown, ok []Finding

	add := func(f Finding) {
		switch f.Level {
		case LevelFault:
			fault = append(fault, f)
		case LevelWarn:
			warn = append(warn, f)
		case LevelUnknown:
			unknown = append(unknown, f)
		default:
			ok = append(ok, f)
		}
	}

	add(integrityFinding(dc))
	add(ftsFinding(dc))
	add(backupFinding(m))
	add(diskFinding(m))
	add(crashFinding(m))
	add(walFinding(m))
	add(bloatFinding(m, dc))

	return append(append(append(fault, warn...), unknown...), ok...)
}

// integrityFinding reports PRAGMA integrity_check against the most recent
// snapshot. This is the main-database check and is trustworthy — unlike the FTS
// integrity-check, which fires on healthy databases and is not run at all.
func integrityFinding(dc *plex.DeepCheck) Finding {
	if dc == nil {
		return Finding{LevelUnknown, "Integrity not yet checked",
			"the first deep check has not completed"}
	}

	if dc.Err != "" {
		// A check that could not run tells us nothing about the database.
		return Finding{LevelUnknown, "Integrity check could not run", dc.Err}
	}

	age := time.Since(dc.StartedAt)
	when := fmt.Sprintf("checked %s ago in %.0fs", roundDuration(age), dc.Duration.Seconds())

	if !dc.IntegrityOK {
		detail := dc.IntegrityDetail
		if detail == "" {
			detail = "integrity_check did not return ok"
		}
		// The most serious thing pledebe can find: the database itself is
		// damaged. Red is reserved for states like this so it keeps meaning.
		return Finding{LevelFault, "Database integrity check FAILED", detail}
	}

	if age > integrityStaleAfter {
		return Finding{LevelUnknown, "Integrity check is stale",
			fmt.Sprintf("last passed %s ago", roundDuration(age))}
	}

	return Finding{LevelOK, "Database integrity verified", when}
}

// ftsFinding reports full-text index health.
//
// PRAGMA integrity_check does not cover FTS indexes: they can be corrupt while
// the main check returns ok. DBRepair documents the consequence — adding an
// item to a collection or updating metadata fails with "database disk image is
// malformed" — and its Reindex rebuilds them.
//
// The detail deliberately states that reads are unaffected. During development
// this was nearly dismissed as a false positive precisely because search kept
// working; the documented symptom is on WRITES. Telling the user that up front
// stops them concluding we are wrong when their searches still return results.
func ftsFinding(dc *plex.DeepCheck) Finding {
	if dc == nil || dc.Err != "" || len(dc.FTS) == 0 {
		return Finding{LevelUnknown, "Search indexes not yet checked",
			"the first deep check has not reported full-text index health"}
	}

	var failed []string
	var missing int64
	for _, t := range dc.FTS {
		if !t.IntegrityOK {
			failed = append(failed, t.Name)
		}
		missing += t.MissingDocs()
	}

	if len(failed) > 0 {
		return Finding{LevelWarn, "Search indexes report corruption",
			fmt.Sprintf("%d of %d full-text indexes failed their integrity check (%s). "+
				"Searching still works — the documented symptom is adding items to "+
				"collections or editing metadata failing. DBRepair's Reindex rebuilds them.",
				len(failed), len(dc.FTS), strings.Join(failed, ", "))}
	}

	if missing > 0 {
		return Finding{LevelUnknown, "Search indexes are incomplete",
			fmt.Sprintf("%d documents missing across %d indexes, though all pass their "+
				"integrity check", missing, len(dc.FTS))}
	}

	return Finding{LevelOK, "Search indexes healthy",
		fmt.Sprintf("%d full-text indexes pass integrity check", len(dc.FTS))}
}

func roundDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
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
		return Finding{LevelFault, "Not enough free space for a snapshot",
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
//
// Once a deep check has run we have the exact figure and use that instead.
func bloatFinding(m *plex.Metrics, dc *plex.DeepCheck) Finding {
	if dc != nil && dc.Err == "" && dc.SnapshotBytes > 0 {
		pct := float64(dc.ReclaimableBytes) / float64(dc.DatabaseBytes) * 100
		detail := fmt.Sprintf("%s reclaimable of %s (%.1f%%), measured exactly",
			humanBytes(dc.ReclaimableBytes), humanBytes(dc.DatabaseBytes), pct)
		if pct > 25 {
			return Finding{LevelUnknown, "Database is bloated", detail}
		}
		return Finding{LevelOK, "No significant bloat", detail}
	}

	if m.FreeRatio() > 0.30 {
		return Finding{LevelUnknown, "Database may be bloated",
			fmt.Sprintf("%.0f%% free pages -- a deep check would measure it exactly", m.FreeRatio()*100)}
	}
	return Finding{LevelOK, "No significant bloat",
		fmt.Sprintf("%.1f%% free pages (floor only)", m.FreeRatio()*100)}
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
