package health

import (
	"fmt"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// Recommendation is shown only when DBRepair would actually help.
//
// Deliberately narrow. Stale backups, low disk space, crashes and slow queries
// are all worth reporting, and DBRepair fixes none of them — putting a "run
// DBRepair" banner above those would train people to ignore it. Two conditions
// qualify: the database failing its integrity check, and corrupt full-text
// indexes.
type Recommendation struct {
	// Action names the DBRepair menu option to choose.
	Action string
	Level  Level

	// Why states the problem in the user's terms.
	Why string

	// Steps are ordered and include the destructive-operation warnings.
	Steps []string

	// Blocked, when set, means the action must NOT be attempted yet — most
	// often because there is not enough free space to survive it. The banner
	// shows this instead of the steps.
	Blocked string

	// Caution is a consequence the user must understand before starting.
	Caution string
}

// repairHeadroom is how much free space a repair is assumed to need.
//
// PROVISIONAL -- see docs/thresholds.md. This gates a destructive operation and
// has not been verified against DBRepair's actual requirements, so it is the
// one provisional threshold that could cause harm rather than noise.
const repairHeadroom = 3

// Recommend returns the single most important DBRepair action, or nil when
// nothing needs one.
func Recommend(m *plex.Metrics, dc *plex.DeepCheck) *Recommendation {
	if dc == nil || dc.Err != "" {
		return nil // nothing verified yet; recommending a repair would be a guess
	}

	if !dc.IntegrityOK {
		return integrityRecommendation(m, dc)
	}
	if ftsCorrupt(dc) {
		return reindexRecommendation()
	}
	return nil
}

func ftsCorrupt(dc *plex.DeepCheck) bool {
	for _, t := range dc.FTS {
		if !t.IntegrityOK {
			return true
		}
	}
	return false
}

// integrityRecommendation prefers restoring a recent backup over repairing.
//
// A repair salvages what it can from a damaged file; a restore returns a file
// that was known-good. When a recent backup exists, restoring is the better
// outcome and users frequently do not realise they have the option.
func integrityRecommendation(m *plex.Metrics, dc *plex.DeepCheck) *Recommendation {
	r := &Recommendation{Level: LevelFault}

	if m != nil && m.BackupCount > 0 && !m.NewestBackupAt.IsZero() &&
		time.Since(m.NewestBackupAt) < backupStaleAfter {

		age := int(time.Since(m.NewestBackupAt).Hours() / 24)
		r.Action = "Replace — restore from backup"
		r.Why = fmt.Sprintf(
			"Your database failed its integrity check, and you have a backup from %s (%d days old). "+
				"Restoring a file that was known-good beats repairing a damaged one.",
			m.NewestBackupAt.Format("2 January"), age)
		r.Caution = fmt.Sprintf(
			"Anything added or changed in the last %d days will be lost. If that matters, "+
				"try Repair first and keep the backup as a fallback.", age)
		r.Steps = []string{
			"Stop Plex Media Server.",
			"Run DBRepair and choose Replace.",
			"Pick the backup dated " + m.NewestBackupAt.Format("2006-01-02") + ".",
			"Start Plex, then run a deep check here to confirm.",
		}
	} else {
		r.Action = "Repair"
		r.Why = "Your database failed its integrity check. DBRepair's Repair extracts " +
			"what it can and rebuilds the file."
		r.Caution = "No recent backup was found, so there is nothing to fall back on. " +
			"Copy your database somewhere safe before starting."
		r.Steps = []string{
			"Stop Plex Media Server.",
			"Copy com.plexapp.plugins.library.db somewhere safe.",
			"Run DBRepair and choose Repair, or Automatic to check, repair and reindex in one pass.",
			"Start Plex, then run a deep check here to confirm.",
		}
	}

	// Gate on space before telling anyone to start. Running out partway through
	// a repair is the worst outcome available.
	if m != nil && m.VolumeFreeBytes > 0 && dc.DatabaseBytes > 0 {
		needed := dc.DatabaseBytes * repairHeadroom
		if m.VolumeFreeBytes < needed {
			r.Blocked = fmt.Sprintf(
				"Do not start yet: a repair needs roughly %s free and only %s is available "+
					"on the Plex metadata volume. Free space first — running out partway "+
					"through is worse than the fault you are fixing.",
				humanBytes(needed), humanBytes(m.VolumeFreeBytes))
		}
	}

	return r
}

func reindexRecommendation() *Recommendation {
	return &Recommendation{
		Action: "Reindex",
		Level:  LevelWarn,
		Why: "Your full-text search indexes report corruption. Searching still works, " +
			"so this is easy to miss — the symptom is adding items to a collection or " +
			"editing metadata failing. Plex's own integrity check cannot see it.",
		Caution: "Non-destructive: Reindex rebuilds the search indexes and changes nothing else.",
		Steps: []string{
			"Stop Plex Media Server.",
			"Run DBRepair and choose Reindex.",
			"Start Plex, then run a deep check here to confirm.",
		},
	}
}
