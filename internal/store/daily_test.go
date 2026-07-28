package store

import (
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
)

func TestRollupAggregatesADay(t *testing.T) {
	st := openTemp(t)
	day := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)

	for i, s := range []struct{ wal, free, db int64 }{
		{4_000_000, 700_000_000_000, 1_000},
		{18_000_000, 650_000_000_000, 1_100}, // WAL peak, free-space trough
		{5_000_000, 690_000_000_000, 1_200},  // last sample wins for point values
	} {
		m := &plex.Metrics{
			CollectedAt:     day.Add(time.Duration(i) * time.Hour),
			WALBytes:        s.wal,
			VolumeFreeBytes: s.free,
			DatabaseBytes:   s.db,
			PMSVersion:      "1.43.3.10828",
		}
		if err := st.Insert(m); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.RollupDay(day); err != nil {
		t.Fatalf("RollupDay: %v", err)
	}

	days, err := st.RecentDays(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 {
		t.Fatalf("got %d daily rows, want 1", len(days))
	}
	d := days[0]

	if d.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", d.SampleCount)
	}
	// The extreme is the interesting number for these two.
	if d.WALBytesMax != 18_000_000 {
		t.Errorf("WALBytesMax = %d, want the day's peak", d.WALBytesMax)
	}
	if d.VolumeFreeMin != 650_000_000_000 {
		t.Errorf("VolumeFreeMin = %d, want the day's trough", d.VolumeFreeMin)
	}
	// Point values come from the last sample.
	if d.DatabaseBytes != 1_200 {
		t.Errorf("DatabaseBytes = %d, want the last sample's value", d.DatabaseBytes)
	}
}

func TestRollupIsIdempotent(t *testing.T) {
	st := openTemp(t)
	day := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if err := st.Insert(&plex.Metrics{CollectedAt: day, DatabaseBytes: 5}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := st.RollupDay(day); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.DayCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("DayCount = %d, want 1 -- repeated rollups must replace, not duplicate", n)
	}
}

// The point of tiered retention: raw samples expire, daily history does not.
func TestPruneRawKeepsDailyForever(t *testing.T) {
	st := openTemp(t)

	old := time.Now().UTC().AddDate(0, 0, -200)
	recent := time.Now().UTC()
	for _, at := range []time.Time{old, recent} {
		if err := st.Insert(&plex.Metrics{CollectedAt: at, DatabaseBytes: 42}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.PruneRaw(14 * 24 * time.Hour); err != nil {
		t.Fatalf("PruneRaw: %v", err)
	}

	raw, err := st.Count()
	if err != nil {
		t.Fatal(err)
	}
	if raw != 1 {
		t.Errorf("raw samples = %d, want 1 (the 200-day-old one should be gone)", raw)
	}

	days, err := st.DayCount()
	if err != nil {
		t.Fatal(err)
	}
	if days != 1 {
		t.Errorf("daily rows = %d, want 1 -- the old day must survive as a rollup", days)
	}
}
