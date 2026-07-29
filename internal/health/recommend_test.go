package health

import (
	"strings"
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// The banner must be rare, or people stop reading it. DBRepair fixes neither
// stale backups nor low disk nor crashes, so none of those may summon it.
func TestNoRecommendationForThingsDBRepairCannotFix(t *testing.T) {
	healthyDeep := &plex.DeepCheck{StartedAt: time.Now(), IntegrityOK: true,
		FTS: []plex.FTSTable{{Name: "fts4_metadata_titles", IntegrityOK: true}}}

	cases := map[string]*plex.Metrics{
		"stale backups":  {BackupCount: 1, BackupDirVisible: true, NewestBackupAt: time.Now().Add(-90 * 24 * time.Hour)},
		"low disk":       {DatabaseBytes: 1 << 30, VolumeFreeBytes: 1 << 20},
		"recent crashes": {RecentCrashCount: 12},
		"large WAL":      {WALBytes: 4 << 30},
	}

	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if r := Recommend(m, healthyDeep); r != nil {
				t.Errorf("recommended %q for a problem DBRepair does not fix", r.Action)
			}
		})
	}
}

// Nothing verified yet means nothing to recommend. Suggesting a repair off the
// back of no evidence would be the worst possible false positive.
func TestNoRecommendationWithoutADeepCheck(t *testing.T) {
	if r := Recommend(&plex.Metrics{}, nil); r != nil {
		t.Errorf("recommended %q with no deep check", r.Action)
	}
	failed := &plex.DeepCheck{StartedAt: time.Now(), Err: "not enough scratch space"}
	if r := Recommend(&plex.Metrics{}, failed); r != nil {
		t.Errorf("recommended %q from a check that could not run", r.Action)
	}
}

func TestReindexRecommendedForCorruptFTS(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt: time.Now(), IntegrityOK: true,
		FTS: []plex.FTSTable{{Name: "fts4_tag_titles_icu", IntegrityOK: false}},
	}

	r := Recommend(&plex.Metrics{}, dc)
	if r == nil || r.Action != "Reindex" {
		t.Fatalf("got %+v, want a Reindex recommendation", r)
	}
	if r.Level != LevelWarn {
		t.Errorf("Level = %q, want %q", r.Level, LevelWarn)
	}
	// Users test search, find it working, and doubt the tool.
	if !strings.Contains(r.Why, "Searching still works") {
		t.Error("Why must explain that reads are unaffected")
	}
	if !strings.Contains(r.Caution, "Non-destructive") {
		t.Error("Reindex is safe and should say so, or people will not run it")
	}
}

// A known-good backup beats salvaging a damaged file, and users often do not
// realise they have the option.
func TestRestorePreferredOverRepairWhenBackupIsRecent(t *testing.T) {
	m := &plex.Metrics{
		BackupCount: 4, BackupDirVisible: true,
		NewestBackupAt:  time.Now().Add(-24 * time.Hour),
		DatabaseBytes:   1 << 30,
		VolumeFreeBytes: 100 << 30,
	}
	dc := &plex.DeepCheck{StartedAt: time.Now(), IntegrityOK: false, DatabaseBytes: 1 << 30}

	r := Recommend(m, dc)
	if r == nil || !strings.Contains(r.Action, "Replace") {
		t.Fatalf("got %+v, want a Replace/restore recommendation", r)
	}
	if r.Level != LevelFault {
		t.Errorf("Level = %q, want %q", r.Level, LevelFault)
	}
	// Restoring silently loses recent changes; saying so is not optional.
	if !strings.Contains(r.Caution, "will be lost") {
		t.Error("Caution must state that recent changes are lost")
	}
}

func TestRepairWhenNoUsableBackup(t *testing.T) {
	m := &plex.Metrics{DatabaseBytes: 1 << 30, VolumeFreeBytes: 100 << 30}
	dc := &plex.DeepCheck{StartedAt: time.Now(), IntegrityOK: false, DatabaseBytes: 1 << 30}

	r := Recommend(m, dc)
	if r == nil || r.Action != "Repair" {
		t.Fatalf("got %+v, want a Repair recommendation", r)
	}
	if !strings.Contains(r.Caution, "nothing to fall back on") {
		t.Error("Caution must point out there is no backup")
	}
}

// Running out of space partway through a repair is worse than the fault being
// repaired, so the banner must refuse rather than instruct.
func TestRepairBlockedWithoutHeadroom(t *testing.T) {
	m := &plex.Metrics{DatabaseBytes: 10 << 30, VolumeFreeBytes: 2 << 30}
	dc := &plex.DeepCheck{StartedAt: time.Now(), IntegrityOK: false, DatabaseBytes: 10 << 30}

	r := Recommend(m, dc)
	if r == nil {
		t.Fatal("expected a recommendation")
	}
	if r.Blocked == "" {
		t.Fatal("expected the action to be blocked for lack of space")
	}
	if !strings.Contains(r.Blocked, "Do not start yet") {
		t.Errorf("Blocked = %q, want an unambiguous instruction not to proceed", r.Blocked)
	}
}

// Integrity failure outranks FTS corruption when both are present.
func TestIntegrityOutranksFTS(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt: time.Now(), IntegrityOK: false, DatabaseBytes: 1 << 30,
		FTS: []plex.FTSTable{{Name: "fts4_metadata_titles", IntegrityOK: false}},
	}
	r := Recommend(&plex.Metrics{VolumeFreeBytes: 100 << 30, DatabaseBytes: 1 << 30}, dc)
	if r == nil || r.Action == "Reindex" {
		t.Fatalf("got %+v, want the integrity action to win", r)
	}
}

// Everything healthy means no banner at all.
func TestNoBannerWhenHealthy(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt: time.Now(), IntegrityOK: true,
		FTS: []plex.FTSTable{{Name: "fts4_metadata_titles", IntegrityOK: true}},
	}
	if r := Recommend(&plex.Metrics{VolumeFreeBytes: 100 << 30}, dc); r != nil {
		t.Errorf("recommended %q on a healthy server", r.Action)
	}
}
