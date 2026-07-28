package health

import (
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

	f, ok := find(Evaluate(m), "Backup freshness unknown")
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

	f, ok := find(Evaluate(m), "No database backups found")
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
			for _, f := range Evaluate(m) {
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
		{"smaller than the database", dbSize / 2, LevelWarn},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &plex.Metrics{DatabaseBytes: dbSize, VolumeFreeBytes: tc.free}
			var got Level
			for _, f := range Evaluate(m) {
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

	for _, f := range Evaluate(m) {
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

	findings := Evaluate(m)
	seenNonWarn := false
	for _, f := range findings {
		if f.Level != LevelWarn {
			seenNonWarn = true
		} else if seenNonWarn {
			t.Fatalf("warning %q appears after a lower-severity finding", f.Title)
		}
	}
}
