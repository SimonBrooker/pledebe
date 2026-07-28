package health

import (
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// MetricLevels grades individual measurements so the page can colour them.
//
// Two rules keep this honest:
//
//   - Only values with a defined healthy range appear here. Page size, Plex
//     version and byte counts with no threshold are facts, not measurements;
//     colouring them green would imply a judgement that does not exist and
//     would dilute the green that does.
//   - The thresholds are the ones the findings use. If a tile were graded
//     independently, the page could show a green value beside an amber finding
//     about the same thing and contradict itself.
//
// Keys are stable identifiers used by the template.
func MetricLevels(m *plex.Metrics, dc *plex.DeepCheck) map[string]Level {
	levels := map[string]Level{}
	if m == nil {
		return levels
	}

	// Write-ahead log: same threshold as walFinding.
	if m.WALBytes > walLarge {
		levels["wal"] = LevelWarn
	} else {
		levels["wal"] = LevelOK
	}

	// Free pages: never worse than a prompt to measure properly, matching
	// bloatFinding, which may not warn.
	if m.FreeRatio() > 0.30 {
		levels["freepages"] = LevelWarn
	} else {
		levels["freepages"] = LevelOK
	}

	// Free space: red when a snapshot could not even be taken.
	switch {
	case m.VolumeFreeBytes == 0:
		// Unmeasured, so ungraded rather than green.
	case m.VolumeFreeBytes < m.DatabaseBytes:
		levels["volumefree"] = LevelFault
	case m.VolumeFreeBytes < m.DatabaseBytes*3:
		levels["volumefree"] = LevelWarn
	default:
		levels["volumefree"] = LevelOK
	}

	// Backups: graded only when we can actually see them.
	if m.BackupDirVisible || m.BackupCount > 0 {
		if m.BackupCount == 0 {
			levels["backupcount"] = LevelWarn
		} else {
			levels["backupcount"] = LevelOK
		}
		if !m.NewestBackupAt.IsZero() {
			if time.Since(m.NewestBackupAt) > backupStaleAfter {
				levels["backupage"] = LevelWarn
			} else {
				levels["backupage"] = LevelOK
			}
		}
	}

	// Recent crashes. The all-time total is deliberately ungraded: it counts
	// every Plex version ever installed and says nothing about today.
	if m.RecentCrashCount > 0 {
		levels["crashes14d"] = LevelWarn
	} else {
		levels["crashes14d"] = LevelOK
	}

	if dc != nil && dc.Err == "" {
		if dc.IntegrityOK {
			levels["integrity"] = LevelOK
		} else {
			levels["integrity"] = LevelFault
		}
		if dc.SnapshotBytes > 0 {
			if float64(dc.ReclaimableBytes)/float64(dc.DatabaseBytes) > 0.25 {
				levels["reclaimable"] = LevelWarn
			} else {
				levels["reclaimable"] = LevelOK
			}
		}
	}

	return levels
}
