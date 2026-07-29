package plex

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
)

// crashExcerptBytes bounds how much of a crash log we read.
//
// Observed sizes on a real server range from 30 KB to 6 MB. The useful part is
// always the end — the lines immediately before the process died — so we seek
// rather than read the file.
const crashExcerptBytes = 64 << 10

// crashExcerptLines is how many lines to keep from that tail.
const crashExcerptLines = 40

// secretPattern redacts credentials from Plex log output.
//
// PMS writes request URLs into its logs, and those URLs carry X-Plex-Token.
// Rendering a crash log verbatim would publish the server's token on a web
// page that may have no authentication. This is not optional.
// The separator group deliberately allows whitespace and quotes: logs contain
// `X-Plex-Token=abc`, `PlexOnlineToken="abc"` and `token: abc` alike, and a
// pattern that only matched the first would leak the other two.
var secretPattern = regexp.MustCompile(
	`(?i)\b(X-Plex-Token|PlexOnlineToken|token|password)(["']?\s*[=:]\s*["']?)([A-Za-z0-9_\-]{8,})`)

// redactSecrets replaces credential values with a marker, keeping the key so
// the reader can still see what was there.
func redactSecrets(s string) string {
	return secretPattern.ReplaceAllString(s, "$1$2[REDACTED]")
}

// isCrashFile reports whether a filename looks like a crash report.
//
// Plex writes Breakpad minidumps (.dmp) and, on newer versions, a text log
// alongside or instead (.dmp.log). The directory also contains .RateLimit.json,
// which is the crash uploader's own state and must never be counted as a crash
// -- it is rewritten constantly, so counting it would report a crash today,
// every day, forever.
func isCrashFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".dmp") || strings.HasSuffix(lower, ".dmp.log")
}

// hasReadableLog reports whether a crash file contains text we can show. A bare
// .dmp is a binary minidump: displaying it raw would be noise.
func hasReadableLog(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".dmp.log")
}

// crashExcerpt returns the last lines of a crash log, redacted.
//
// Reads at most crashExcerptBytes from the end of the file, so a 6 MB log costs
// the same as a 30 KB one.
func crashExcerpt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}

	offset := int64(0)
	if info.Size() > crashExcerptBytes {
		offset = info.Size() - crashExcerptBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	// Ring buffer: keep only the last N lines without holding the whole tail.
	lines := make([]string, 0, crashExcerptLines)
	for scanner.Scan() {
		if len(lines) == crashExcerptLines {
			lines = lines[1:]
		}
		lines = append(lines, scanner.Text())
	}

	// The first line is probably a fragment, since we seeked into the middle of
	// the file rather than to a line boundary.
	if offset > 0 && len(lines) > 1 {
		lines = lines[1:]
	}

	return redactSecrets(strings.TrimSpace(strings.Join(lines, "\n")))
}
