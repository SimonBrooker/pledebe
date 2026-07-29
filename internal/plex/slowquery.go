package plex

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// PMS logs its own slow queries, which makes this one of the few signals
// pledebe does not have to invent a threshold for: Plex decides what counts as
// slow. Observed format:
//
//	Jul 29, 2026 03:08:01.559 [2241...] WARN - [Req#e3450] SLOW QUERY: It took 460.000000 ms to retrieve 48 items.
//
// Note there is no SQL in the line — only a duration and an item count. Nothing
// here needs redacting, unlike crash logs.
var slowQueryPattern = regexp.MustCompile(
	`^(\w{3} \d{1,2}, \d{4} \d{2}:\d{2}:\d{2}\.\d{3}).*SLOW QUERY: It took ([\d.]+) ms to retrieve (\d+) items`)

// plexLogTime matches the timestamp format above.
const plexLogTime = "Jan 2, 2006 15:04:05.000"

// maxLogBytesPerFile bounds how much of any one log file is read. With debug
// logging on, PMS rotates 10 MB files every hour or so; reading the whole set
// every poll would be wasteful for no benefit, since we only want new lines.
const maxLogBytesPerFile = 12 << 20

// SlowQueries summarises PMS's own slow-query warnings over a window.
//
// Deliberately NOT graded green/amber/red. 725 warnings in six hours may be
// entirely normal for a large library, or a symptom — there is no baseline yet,
// and inventing a threshold is exactly the mistake this project keeps catching.
// Measure first.
type SlowQueries struct {
	// Since is the oldest log line considered, so a rate can be computed
	// honestly rather than assuming the window was fully covered.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	Count int `json:"count"`

	// Durations in milliseconds, as PMS reported them.
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	Max float64 `json:"max_ms"`

	TotalItems int64 `json:"total_items"`

	// ButlerStart and ButlerEnd are the server's actual configured maintenance
	// hours, carried here so the page can name the real window rather than
	// asserting PMS's defaults at a reader who changed them.
	ButlerStart int `json:"butler_start"`
	ButlerEnd   int `json:"butler_end"`

	// InButlerWindow counts warnings logged during Plex's scheduled maintenance
	// hours. Slow queries at 3am while Butler is running are expected; the same
	// rate at 8pm is not, and a single number would hide the difference.
	InButlerWindow int `json:"in_butler_window"`
}

// PerHour returns the rate across the observed window, or 0 if the window is
// too short to be meaningful.
func (s SlowQueries) PerHour() float64 {
	if s.Count == 0 {
		return 0
	}
	hours := s.Until.Sub(s.Since).Hours()
	if hours < 0.1 {
		return 0
	}
	return float64(s.Count) / hours
}

// MsPerItem normalises duration by result size: a 460 ms query returning 48
// rows is not the same as a 460 ms query returning 1.
func (s SlowQueries) MsPerItem() float64 {
	if s.TotalItems == 0 {
		return 0
	}
	var total float64
	// P50 x Count approximates total time well enough for a ratio; PMS does not
	// log a total and summing every duration is not worth storing.
	total = s.P50 * float64(s.Count)
	return total / float64(s.TotalItems)
}

// collectSlowQueries scans PMS logs for slow-query warnings newer than since.
//
// Only lines matching the pattern are parsed, and files untouched since the
// cutoff are skipped entirely, so this costs a sequential read of the recent
// logs and nothing else.
func (in *Install) collectSlowQueries(since time.Time, butlerStart, butlerEnd int) *SlowQueries {
	if in.LogDir == "" {
		return nil
	}

	// PMS rotates as "Plex Media Server.log", ".1.log" … ".5.log". An earlier
	// version of this project globbed "Plex Media Server.log*", which matches
	// only the current file and silently searched none of the rotations.
	paths, err := filepath.Glob(filepath.Join(in.LogDir, "Plex Media Server*.log"))
	if err != nil || len(paths) == 0 {
		return nil
	}

	sq := &SlowQueries{ButlerStart: butlerStart, ButlerEnd: butlerEnd}
	var durations []float64

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || (!since.IsZero() && info.ModTime().Before(since)) {
			continue // wholly older than the window
		}
		parseSlowQueryFile(path, since, butlerStart, butlerEnd, sq, &durations)
	}

	if len(durations) == 0 {
		return nil
	}

	sort.Float64s(durations)
	sq.Count = len(durations)
	sq.P50 = percentileOf(durations, 0.50)
	sq.P95 = percentileOf(durations, 0.95)
	sq.Max = durations[len(durations)-1]

	return sq
}

func parseSlowQueryFile(path string, since time.Time, butlerStart, butlerEnd int,
	sq *SlowQueries, durations *[]float64) {

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.Size() > maxLogBytesPerFile {
		_, _ = f.Seek(info.Size()-maxLogBytesPerFile, 0)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		m := slowQueryPattern.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		at, err := time.ParseInLocation(plexLogTime, m[1], time.Local)
		if err != nil || (!since.IsZero() && !at.After(since)) {
			continue
		}
		ms, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		items, _ := strconv.ParseInt(m[3], 10, 64)

		*durations = append(*durations, ms)
		sq.TotalItems += items

		if sq.Since.IsZero() || at.Before(sq.Since) {
			sq.Since = at
		}
		if at.After(sq.Until) {
			sq.Until = at
		}
		if inButlerWindow(at, butlerStart, butlerEnd) {
			sq.InButlerWindow++
		}
	}
}

// inButlerWindow reports whether t falls in Plex's scheduled maintenance hours,
// handling a window that wraps past midnight.
func inButlerWindow(t time.Time, start, end int) bool {
	if start == end {
		return false
	}
	h := t.Hour()
	if start < end {
		return h >= start && h < end
	}
	return h >= start || h < end
}

// percentileOf expects a sorted slice.
func percentileOf(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
