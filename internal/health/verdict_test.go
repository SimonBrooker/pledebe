package health

import (
	"strings"
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

func find(findings []Finding, title string) (Finding, bool) {
	for _, f := range findings {
		if f.Title == title {
			return f, true
		}
	}
	return Finding{}, false
}

// The false alarm that actually shipped: backups were reported missing when
// pledebe simply could not see the directory PMS writes them to. That must be
// Unknown, never Warn.
func TestInvisibleBackupDirIsUnknownNotWarning(t *testing.T) {
	m := &plex.Metrics{
		BackupCount:       0,
		BackupDirVisible:  false,
		BackupDirExpected: "/backup/Databases",
	}

	f, ok := find(Evaluate(m, nil), "Backup freshness unknown")
	if !ok {
		t.Fatal("expected an unknown finding for an unreachable backup directory")
	}
	if f.Level != LevelUnknown {
		t.Errorf("Level = %q, want %q — absence of data is not evidence of failure", f.Level, LevelUnknown)
	}
}

// A visible directory with genuinely no backups IS a fault.
func TestVisibleButEmptyBackupDirWarns(t *testing.T) {
	m := &plex.Metrics{BackupCount: 0, BackupDirVisible: true, BackupDirSearched: "/plexbackups"}

	f, ok := find(Evaluate(m, nil), "No database backups found")
	if !ok || f.Level != LevelWarn {
		t.Errorf("expected a warning; got %+v (ok=%v)", f, ok)
	}
}

func TestBackupFreshness(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want Level
	}{
		{"yesterday", 24 * time.Hour, LevelOK},
		// Plex's default schedule is every three days and a busy server can
		// skip one, so a few days must not warn.
		{"four days", 4 * 24 * time.Hour, LevelOK},
		{"three weeks", 21 * 24 * time.Hour, LevelWarn},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &plex.Metrics{
				BackupCount:      2,
				BackupDirVisible: true,
				NewestBackupAt:   time.Now().Add(-tc.age),
				NewestBackup:     "com.plexapp.plugins.library.db-2026-07-27",
			}
			for _, f := range Evaluate(m, nil) {
				if f.Title == "Database backups current" && tc.want == LevelOK {
					return
				}
				if f.Title == "Database backups are stale" && tc.want == LevelWarn {
					return
				}
			}
			t.Errorf("no finding at level %q for a %s-old backup", tc.want, tc.age)
		})
	}
}

func TestDiskHeadroom(t *testing.T) {
	const dbSize = 1 << 30 // 1 GB

	cases := []struct {
		name string
		free int64
		want Level
	}{
		{"plenty", dbSize * 10, LevelOK},
		{"too tight to repair", dbSize * 2, LevelWarn},
		// Cannot even take a snapshot: broken now, not merely worth attention.
		{"smaller than the database", dbSize / 2, LevelFault},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &plex.Metrics{DatabaseBytes: dbSize, VolumeFreeBytes: tc.free}
			var got Level
			for _, f := range Evaluate(m, nil) {
				if f.Title == "Free space sufficient" ||
					f.Title == "Not enough free space to repair safely" ||
					f.Title == "Not enough free space for a snapshot" {
					got = f.Level
				}
			}
			if got != tc.want {
				t.Errorf("level = %q, want %q", got, tc.want)
			}
		})
	}
}

// The freelist under-reported reclaimable space by 14x in testing, so bloat may
// never produce a warning — only a prompt to measure it properly.
func TestBloatNeverWarns(t *testing.T) {
	m := &plex.Metrics{PageCount: 1000, FreelistCount: 900, PageSize: 4096}

	for _, f := range Evaluate(m, nil) {
		if f.Level == LevelWarn {
			t.Errorf("bloat produced a warning (%q); it must only ever be informational", f.Title)
		}
	}
}

// Warnings must sort above unknowns, and unknowns above healthy signals.
func TestFindingsAreOrderedBySeverity(t *testing.T) {
	m := &plex.Metrics{
		DatabaseBytes:     1 << 30,
		VolumeFreeBytes:   1 << 20, // triggers a warning
		BackupDirVisible:  false,
		BackupDirExpected: "/backup/Databases", // triggers an unknown
		RecentCrashCount:  0,                   // healthy
	}

	findings := Evaluate(m, nil)
	seenNonWarn := false
	for _, f := range findings {
		if f.Level != LevelWarn {
			seenNonWarn = true
		} else if seenNonWarn {
			t.Fatalf("warning %q appears after a lower-severity finding", f.Title)
		}
	}
}

// No deep check yet is Unknown, not a fault. Same for a check that could not
// run: neither tells us anything about the database.
func TestIntegrityUnknownStates(t *testing.T) {
	cases := []struct {
		name  string
		deep  *plex.DeepCheck
		title string
	}{
		{"never run", nil, "Integrity not yet checked"},
		{"could not run",
			&plex.DeepCheck{StartedAt: time.Now(), Err: "need ~1400 MB free in scratch, have 200 MB"},
			"Integrity check could not run"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := find(Evaluate(&plex.Metrics{}, tc.deep), tc.title)
			if !ok {
				t.Fatalf("no finding titled %q", tc.title)
			}
			if f.Level != LevelUnknown {
				t.Errorf("Level = %q, want %q", f.Level, LevelUnknown)
			}
		})
	}
}

// A failed integrity_check is the most serious thing pledebe can report: the
// database itself is damaged. Red is reserved for states like this so that it
// keeps its meaning.
func TestFailedIntegrityIsAFault(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt:       time.Now(),
		IntegrityOK:     false,
		IntegrityDetail: "row 12 missing from index idx_metadata_items",
	}

	f, ok := find(Evaluate(&plex.Metrics{}, dc), "Database integrity check FAILED")
	if !ok || f.Level != LevelFault {
		t.Errorf("expected a fault; got %+v (ok=%v)", f, ok)
	}
}

func TestPassingIntegrityIsOKThenStale(t *testing.T) {
	fresh := &plex.DeepCheck{StartedAt: time.Now(), IntegrityOK: true}
	if f, ok := find(Evaluate(&plex.Metrics{}, fresh), "Database integrity verified"); !ok || f.Level != LevelOK {
		t.Errorf("fresh check: got %+v (ok=%v), want an OK finding", f, ok)
	}

	old := &plex.DeepCheck{StartedAt: time.Now().Add(-5 * 24 * time.Hour), IntegrityOK: true}
	if f, ok := find(Evaluate(&plex.Metrics{}, old), "Integrity check is stale"); !ok || f.Level != LevelUnknown {
		t.Errorf("stale check: got %+v (ok=%v), want an Unknown finding", f, ok)
	}
}

// With a real measurement available, bloat reports the exact figure rather than
// the freelist floor -- but still never warns.
func TestBloatUsesExactMeasurementAndStillNeverWarns(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt:        time.Now(),
		IntegrityOK:      true,
		DatabaseBytes:    1 << 30,
		SnapshotBytes:    1 << 29,
		ReclaimableBytes: 1 << 29, // 50% reclaimable
	}

	for _, f := range Evaluate(&plex.Metrics{}, dc) {
		if f.Title == "Database is bloated" && f.Level == LevelWarn {
			t.Error("bloat must never warn, even when measured exactly")
		}
	}
	if _, ok := find(Evaluate(&plex.Metrics{}, dc), "Database is bloated"); !ok {
		t.Error("expected the exact measurement to be reported")
	}
}

// FTS integrity failure must warn. An earlier version of this project treated
// it as a false positive because reads kept working; DBRepair documents the
// symptom as occurring on writes.
func TestFTSCorruptionWarns(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt:   time.Now(),
		IntegrityOK: true, // main check passes -- FTS damage is invisible to it
		FTS: []plex.FTSTable{
			{Name: "fts4_metadata_titles", IntegrityOK: false, IndexedDocs: 133995, SourceRows: 138181},
			{Name: "fts4_metadata_titles_icu", IntegrityOK: false, IndexedDocs: 138180, SourceRows: 138181},
		},
	}

	f, ok := find(Evaluate(&plex.Metrics{}, dc), "Search indexes report corruption")
	if !ok {
		t.Fatal("expected an FTS corruption finding")
	}
	if f.Level != LevelWarn {
		t.Errorf("Level = %q, want %q", f.Level, LevelWarn)
	}
	// The user will test search, find it working, and doubt us unless we say so.
	if !strings.Contains(f.Detail, "Searching still works") {
		t.Error("detail must explain that reads are unaffected")
	}
	if !strings.Contains(f.Detail, "Reindex") {
		t.Error("detail must name the remedy")
	}
}

func TestFTSHealthyAndIncomplete(t *testing.T) {
	healthy := &plex.DeepCheck{
		StartedAt: time.Now(),
		FTS: []plex.FTSTable{
			{Name: "fts4_metadata_titles", IntegrityOK: true, IndexedDocs: 100, SourceRows: 100},
		},
	}
	if f, ok := find(Evaluate(&plex.Metrics{}, healthy), "Search indexes healthy"); !ok || f.Level != LevelOK {
		t.Errorf("healthy: got %+v (ok=%v)", f, ok)
	}

	// Passing integrity but missing documents is odd, not proven broken.
	incomplete := &plex.DeepCheck{
		StartedAt: time.Now(),
		FTS: []plex.FTSTable{
			{Name: "fts4_tag_titles_icu", IntegrityOK: true, IndexedDocs: 208818, SourceRows: 531055},
		},
	}
	if f, ok := find(Evaluate(&plex.Metrics{}, incomplete), "Search indexes are incomplete"); !ok || f.Level != LevelUnknown {
		t.Errorf("incomplete: got %+v (ok=%v)", f, ok)
	}
}

func TestMissingDocsNeverNegative(t *testing.T) {
	// The ICU index held one MORE document than the source table during
	// testing; subtraction must not underflow into a nonsense figure.
	tbl := plex.FTSTable{IndexedDocs: 138181, SourceRows: 138180}
	if got := tbl.MissingDocs(); got != 0 {
		t.Errorf("MissingDocs = %d, want 0", got)
	}
}

// Regression for a false alarm that survived the first fix and reappeared in
// testing: PMS writes backups to a path pledebe cannot see, stale leftovers sit
// beside the database, and their age was reported as "92 days stale" on a
// server backing up daily.
func TestStaleLeftoversAreNotEvidenceOfStaleBackups(t *testing.T) {
	m := &plex.Metrics{
		BackupCount:            4,
		BackupDirVisible:       true,
		BackupDirAuthoritative: false, // we fell back beside the database
		BackupDirSearched:      "/plexconfig/Plug-in Support/Databases",
		BackupDirExpected:      "/backup/Databases",
		NewestBackupAt:         time.Now().Add(-92 * 24 * time.Hour),
		NewestBackup:           "com.plexapp.plugins.library.db-2026-04-28",
	}

	f, ok := find(Evaluate(m, nil), "Backup freshness unknown")
	if !ok {
		t.Fatal("expected Unknown; leftovers next to the database prove nothing")
	}
	if f.Level != LevelUnknown {
		t.Errorf("Level = %q, want %q", f.Level, LevelUnknown)
	}
	if !strings.Contains(f.Detail, "PLEX_BACKUP_DIR") {
		t.Error("detail must tell the user how to make it measurable")
	}

	// And it must NOT also raise the staleness warning.
	if _, warned := find(Evaluate(m, nil), "Database backups are stale"); warned {
		t.Error("reported staleness from a directory PMS does not write to")
	}
}

// The authoritative directory still warns when genuinely stale.
func TestAuthoritativeDirStillWarnsWhenStale(t *testing.T) {
	m := &plex.Metrics{
		BackupCount:            2,
		BackupDirVisible:       true,
		BackupDirAuthoritative: true,
		BackupDirSearched:      "/plexbackups",
		BackupDirExpected:      "/backup/Databases",
		NewestBackupAt:         time.Now().Add(-92 * 24 * time.Hour),
		NewestBackup:           "com.plexapp.plugins.library.db-2026-04-28",
	}

	f, ok := find(Evaluate(m, nil), "Database backups are stale")
	if !ok || f.Level != LevelWarn {
		t.Errorf("got %+v (ok=%v), want a staleness warning", f, ok)
	}
}

// With no configured path at all, the directory beside the database IS the
// right place to look, and old backups there are genuinely stale.
func TestNoConfiguredPathMeansBesideTheDatabaseIsAuthoritative(t *testing.T) {
	m := &plex.Metrics{
		BackupCount:            2,
		BackupDirVisible:       true,
		BackupDirAuthoritative: true, // collectBackups sets this when unconfigured
		BackupDirSearched:      "/plexconfig/Plug-in Support/Databases",
		NewestBackupAt:         time.Now().Add(-92 * 24 * time.Hour),
	}
	if _, ok := find(Evaluate(m, nil), "Database backups are stale"); !ok {
		t.Error("expected a staleness warning when the fallback is authoritative")
	}
}

// Preferences.xml is mode 0600 on hotio installs. A pledebe running with a
// different PUID cannot read it, so it does not know where PMS writes backups —
// and quietly defaulting produced a confident "92 days stale" on a server
// backing up daily. Verified on a live server 2026-07-29.
func TestUnreadablePreferencesMakesBackupsUnknown(t *testing.T) {
	m := &plex.Metrics{
		BackupCount:            4,
		BackupDirVisible:       true,
		BackupDirAuthoritative: true, // it *looks* authoritative: no path was read
		BackupDirSearched:      "/plexconfig/Plug-in Support/Databases",
		NewestBackupAt:         time.Now().Add(-92 * 24 * time.Hour),
		PreferencesNote:        "cannot read Preferences.xml: owned by uid 1000, running as uid 99",
	}

	f, ok := find(Evaluate(m, nil), "Backup freshness unknown")
	if !ok || f.Level != LevelUnknown {
		t.Fatalf("got %+v (ok=%v), want Unknown", f, ok)
	}
	// The message has to carry the fix, not just the symptom.
	if !strings.Contains(f.Detail, "uid") {
		t.Error("detail must include the ownership problem so the user can fix it")
	}
	if _, warned := find(Evaluate(m, nil), "Database backups are stale"); warned {
		t.Error("reported staleness while blind to where backups are written")
	}
}

// Readable preferences must not be affected.
func TestReadablePreferencesStillWarnsOnStale(t *testing.T) {
	m := &plex.Metrics{
		BackupCount:            2,
		BackupDirVisible:       true,
		BackupDirAuthoritative: true,
		BackupDirSearched:      "/plexbackups",
		NewestBackupAt:         time.Now().Add(-92 * 24 * time.Hour),
	}
	if _, ok := find(Evaluate(m, nil), "Database backups are stale"); !ok {
		t.Error("expected a staleness warning when preferences were readable")
	}
}
