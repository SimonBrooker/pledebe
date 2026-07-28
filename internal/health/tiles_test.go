package health

import (
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// Values without a healthy range must stay ungraded. Colouring a fact green
// implies a judgement that does not exist, and dilutes the green that does.
func TestFactsAreNotGraded(t *testing.T) {
	levels := MetricLevels(&plex.Metrics{
		DatabaseBytes: 1 << 30, PageCount: 291757, PageSize: 4096,
		BlobsBytes: 3 << 30, SHMBytes: 32768, CrashReportCount: 11,
		VolumeFreeBytes: 100 << 30,
	}, nil)

	for _, key := range []string{"database", "blobs", "pages", "pagesize", "shm", "crashes_alltime"} {
		if _, graded := levels[key]; graded {
			t.Errorf("%q has no healthy range and must not be coloured", key)
		}
	}
}

// A tile and the finding about the same thing must never disagree.
func TestTileLevelsAgreeWithFindings(t *testing.T) {
	cases := []struct {
		name  string
		m     *plex.Metrics
		key   string
		title string
	}{
		{
			name:  "stale backup",
			m:     &plex.Metrics{BackupCount: 2, BackupDirVisible: true, NewestBackupAt: time.Now().Add(-30 * 24 * time.Hour)},
			key:   "backupage",
			title: "Database backups are stale",
		},
		{
			name:  "current backup",
			m:     &plex.Metrics{BackupCount: 4, BackupDirVisible: true, NewestBackupAt: time.Now().Add(-24 * time.Hour)},
			key:   "backupage",
			title: "Database backups current",
		},
		{
			name:  "no room for a snapshot",
			m:     &plex.Metrics{DatabaseBytes: 1 << 30, VolumeFreeBytes: 1 << 20},
			key:   "volumefree",
			title: "Not enough free space for a snapshot",
		},
		{
			name:  "recent crashes",
			m:     &plex.Metrics{RecentCrashCount: 3},
			key:   "crashes14d",
			title: "Recent Plex crashes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := find(Evaluate(tc.m, nil), tc.title)
			if !ok {
				t.Fatalf("no finding titled %q", tc.title)
			}
			if got := MetricLevels(tc.m, nil)[tc.key]; got != f.Level {
				t.Errorf("tile %q is %q but the finding is %q — the page would contradict itself",
					tc.key, got, f.Level)
			}
		})
	}
}

// An unmeasurable value is ungraded, not green. Green asserts health.
func TestUnmeasuredValuesAreUngraded(t *testing.T) {
	levels := MetricLevels(&plex.Metrics{VolumeFreeBytes: 0}, nil)
	if _, graded := levels["volumefree"]; graded {
		t.Error("free space could not be read, so it must not be coloured")
	}

	// Backups invisible to pledebe: unmeasured, so ungraded.
	levels = MetricLevels(&plex.Metrics{BackupDirVisible: false, BackupCount: 0}, nil)
	if _, graded := levels["backupcount"]; graded {
		t.Error("an unreachable backup directory must not be graded")
	}
}

func TestIntegrityTileIsFaultWhenFailed(t *testing.T) {
	dc := &plex.DeepCheck{StartedAt: time.Now(), IntegrityOK: false}
	if got := MetricLevels(&plex.Metrics{}, dc)["integrity"]; got != LevelFault {
		t.Errorf("integrity tile = %q, want %q", got, LevelFault)
	}

	dc.IntegrityOK = true
	if got := MetricLevels(&plex.Metrics{}, dc)["integrity"]; got != LevelOK {
		t.Errorf("integrity tile = %q, want %q", got, LevelOK)
	}
}
