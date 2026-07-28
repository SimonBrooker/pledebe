package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestInsertAndReadBack(t *testing.T) {
	st := openTemp(t)

	m := &plex.Metrics{
		CollectedAt:      time.Now().UTC().Truncate(time.Second),
		DatabaseBytes:    1194328064,
		WALBytes:         4907008,
		BlobsBytes:       3641327616,
		PageCount:        291757,
		PageSize:         4096,
		FreelistCount:    350,
		BackupCount:      4,
		BackupDirVisible: true,
		NewestBackupAt:   time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		PMSVersion:       "1.43.3.10828-00f62d37d",
		CrashesByComponent: map[string]int{
			"PLEX MEDIA SERVER": 2,
		},
	}

	if err := st.Insert(m); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := st.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got == nil {
		t.Fatal("Latest returned nil after an insert")
	}

	if got.DatabaseBytes != m.DatabaseBytes {
		t.Errorf("DatabaseBytes = %d, want %d", got.DatabaseBytes, m.DatabaseBytes)
	}
	if got.PMSVersion != m.PMSVersion {
		t.Errorf("PMSVersion = %q, want %q", got.PMSVersion, m.PMSVersion)
	}
	// Stored as JSON so that fields added later survive in old rows — check a
	// map field makes the round trip.
	if got.CrashesByComponent["PLEX MEDIA SERVER"] != 2 {
		t.Errorf("CrashesByComponent did not round-trip: %v", got.CrashesByComponent)
	}
}

func TestLatestOnEmptyStore(t *testing.T) {
	st := openTemp(t)

	got, err := st.Latest()
	if err != nil {
		t.Fatalf("Latest on an empty store should not error: %v", err)
	}
	if got != nil {
		t.Errorf("Latest = %+v, want nil", got)
	}
}

func TestRecentIsNewestFirst(t *testing.T) {
	st := openTemp(t)

	base := time.Now().UTC().Add(-3 * time.Hour)
	for i := range 3 {
		m := &plex.Metrics{
			CollectedAt:   base.Add(time.Duration(i) * time.Hour),
			DatabaseBytes: int64(i + 1),
		}
		if err := st.Insert(m); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	got, err := st.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3", len(got))
	}
	if got[0].DatabaseBytes != 3 {
		t.Errorf("first sample DatabaseBytes = %d, want the newest (3)", got[0].DatabaseBytes)
	}
}

func TestPruneDropsOldSamples(t *testing.T) {
	st := openTemp(t)

	old := &plex.Metrics{CollectedAt: time.Now().UTC().Add(-100 * 24 * time.Hour)}
	recent := &plex.Metrics{CollectedAt: time.Now().UTC()}
	for _, m := range []*plex.Metrics{old, recent} {
		if err := st.Insert(m); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	if err := st.Prune(90 * 24 * time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	n, err := st.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("Count = %d, want 1 (the old sample should be gone)", n)
	}
}
