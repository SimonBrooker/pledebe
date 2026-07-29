package plex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Real lines captured from a live server, 2026-07-29.
const realLogLines = `Jul 29, 2026 03:08:01.559 [22416339913528] WARN - [Req#e3450] SLOW QUERY: It took 460.000000 ms to retrieve 48 items.
Jul 29, 2026 03:08:11.816 [22416589982520] WARN - [Req#e38ed] SLOW QUERY: It took 230.000000 ms to retrieve 15 items.
Jul 29, 2026 12:30:00.000 [22416116423480] WARN - [Req#e47ea] SLOW QUERY: It took 210.000000 ms to retrieve 15 items.
Jul 29, 2026 12:31:00.000 [22416004729656] WARN - [Req#e4d52] SLOW QUERY: It took 260.000000 ms to retrieve 25 items.
Jul 29, 2026 03:09:00.000 [22416004729656] INFO - [Req#e4d99] Completed request, nothing slow here.
`

func writeLog(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParsesRealSlowQueryLines(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Plex Media Server.log", realLogLines)

	in := &Install{LogDir: dir}
	sq := in.collectSlowQueries(time.Time{}, 2, 8)
	if sq == nil {
		t.Fatal("no slow queries parsed from real log lines")
	}

	if sq.Count != 4 {
		t.Errorf("Count = %d, want 4 (the INFO line must not match)", sq.Count)
	}
	if sq.Max != 460 {
		t.Errorf("Max = %v, want 460", sq.Max)
	}
	if sq.TotalItems != 48+15+15+25 {
		t.Errorf("TotalItems = %d, want 103", sq.TotalItems)
	}
	// Two of the four fall inside the 02:00-08:00 Butler window. Slow queries
	// during scheduled maintenance are expected; conflating them with daytime
	// ones would hide the difference that matters.
	if sq.InButlerWindow != 2 {
		t.Errorf("InButlerWindow = %d, want 2", sq.InButlerWindow)
	}
}

// PMS rotates as "Plex Media Server.1.log", which an earlier glob in this
// project silently failed to match.
func TestScansRotatedLogs(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Plex Media Server.log", realLogLines)
	writeLog(t, dir, "Plex Media Server.1.log", realLogLines)

	in := &Install{LogDir: dir}
	sq := in.collectSlowQueries(time.Time{}, 2, 8)
	if sq == nil || sq.Count != 8 {
		t.Fatalf("rotated logs not scanned: got %+v", sq)
	}
}

// Each poll must count only new lines, or every sample re-counts the whole log.
func TestSinceExcludesAlreadyCountedLines(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Plex Media Server.log", realLogLines)

	cutoff := time.Date(2026, 7, 29, 4, 0, 0, 0, time.Local)
	in := &Install{LogDir: dir}
	sq := in.collectSlowQueries(cutoff, 2, 8)

	if sq == nil {
		t.Fatal("expected the two lines after the cutoff")
	}
	if sq.Count != 2 {
		t.Errorf("Count = %d, want 2 (only the 12:30 and 12:31 lines)", sq.Count)
	}
	if sq.InButlerWindow != 0 {
		t.Errorf("InButlerWindow = %d, want 0", sq.InButlerWindow)
	}
}

func TestNoSlowQueriesReturnsNil(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Plex Media Server.log", "Jul 29, 2026 03:08:00.000 INFO - all quiet\n")

	in := &Install{LogDir: dir}
	if sq := in.collectSlowQueries(time.Time{}, 2, 8); sq != nil {
		t.Errorf("got %+v, want nil when nothing matched", sq)
	}
}

func TestNoLogDirIsNotAnError(t *testing.T) {
	in := &Install{LogDir: ""}
	if sq := in.collectSlowQueries(time.Time{}, 2, 8); sq != nil {
		t.Error("expected nil when there is no log directory")
	}
}

// A window that wraps past midnight is legal in Plex's settings.
func TestButlerWindowWrapsMidnight(t *testing.T) {
	at := time.Date(2026, 7, 29, 23, 30, 0, 0, time.Local)
	if !inButlerWindow(at, 22, 6) {
		t.Error("23:30 should be inside a 22:00-06:00 window")
	}
	if inButlerWindow(time.Date(2026, 7, 29, 12, 0, 0, 0, time.Local), 22, 6) {
		t.Error("12:00 should be outside a 22:00-06:00 window")
	}
}

func TestPercentiles(t *testing.T) {
	dir := t.TempDir()
	var content string
	for i := range 100 {
		content += time.Date(2026, 7, 29, 12, 0, i, 0, time.Local).Format(plexLogTime) +
			" [1] WARN - [Req#x] SLOW QUERY: It took " +
			[]string{"200.000000", "1000.000000"}[i/50] + " ms to retrieve 10 items.\n"
	}
	writeLog(t, dir, "Plex Media Server.log", content)

	in := &Install{LogDir: dir}
	sq := in.collectSlowQueries(time.Time{}, 2, 8)
	if sq == nil {
		t.Fatal("nothing parsed")
	}
	if sq.P50 != 200 {
		t.Errorf("P50 = %v, want 200", sq.P50)
	}
	if sq.P95 != 1000 {
		t.Errorf("P95 = %v, want 1000", sq.P95)
	}
}
