package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

// Storage footprint drives the retention policy: if a year of history is
// trivial, deleting old samples throws away the long-range signal ("grown 40%
// this year", "started after the 1.43.3 upgrade") for no benefit.
//
// Also a regression guard — a field added carelessly to Metrics would show up
// here as a jump in bytes per row.
func TestStorageFootprint(t *testing.T) {
	sample := func(at time.Time) *plex.Metrics {
		return &plex.Metrics{
			CollectedAt:       at,
			DatabaseBytes:     1194328064,
			WALBytes:          4907008,
			SHMBytes:          32768,
			BlobsBytes:        3641327616,
			PageCount:         291757,
			PageSize:          4096,
			FreelistCount:     350,
			FreelistBytes:     1433600,
			NewestBackup:      "com.plexapp.plugins.library.db-2026-07-27",
			NewestBackupAt:    time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
			BackupCount:       4,
			BackupDirSearched: "/plexbackups",
			BackupDirExpected: "/backup/Databases",
			BackupDirVisible:  true,
			PMSVersion:        "1.43.3.10828-00f62d37d",
			VersionSeenAt:     time.Date(2026, 7, 15, 6, 28, 0, 0, time.UTC),
			VolumeFreeBytes:   690835845120,
			CrashesByComponent: map[string]int{
				"PLEX MEDIA SERVER": 0, "PLEX MEDIA SCANNER": 0, "PLEX TUNER SERVICE": 0,
			},
		}
	}

	cases := []struct {
		name string
		rows int
	}{
		{"1 day at 15 min", 96},
		{"90 days at 15 min", 96 * 90},
		{"10 years daily", 3650},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			base := time.Now().UTC().Add(-time.Duration(tc.rows) * time.Hour)
			for i := range tc.rows {
				if err := st.Insert(sample(base.Add(time.Duration(i) * time.Minute))); err != nil {
					t.Fatal(err)
				}
			}

			var total int64
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if info, err := e.Info(); err == nil {
					total += info.Size()
				}
			}
			fmt.Printf("  %-20s %6d rows  %8.2f MB  %5.0f bytes/row\n",
				tc.name, tc.rows, float64(total)/(1<<20), float64(total)/float64(tc.rows))
		})
	}
}
