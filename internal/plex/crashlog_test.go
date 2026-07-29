package plex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plex writes request URLs into its logs, and those URLs carry X-Plex-Token.
// Rendering a crash log verbatim would publish the server's token on a page
// that may have no authentication.
func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		in       string
		mustHide string
	}{
		{`GET /library/sections?X-Plex-Token=abc123XYZ_secret-value HTTP/1.1`, "abc123XYZ_secret-value"},
		{`PlexOnlineToken="s3cr3tt0ken0987"`, "s3cr3tt0ken0987"},
		{`token: aVeryLongSecretValue123`, "aVeryLongSecretValue123"},
		{`password=hunter2hunter2`, "hunter2hunter2"},
	}

	for _, tc := range cases {
		got := redactSecrets(tc.in)
		if strings.Contains(got, tc.mustHide) {
			t.Errorf("redactSecrets(%q) leaked the value: %q", tc.in, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("redactSecrets(%q) = %q, expected a redaction marker", tc.in, got)
		}
	}
}

// The key is kept so a reader can still see what was there.
func TestRedactKeepsTheKey(t *testing.T) {
	got := redactSecrets("X-Plex-Token=abcdefghijklmnop")
	if !strings.Contains(got, "X-Plex-Token") {
		t.Errorf("got %q, expected the key to survive", got)
	}
}

// .RateLimit.json is the crash uploader's own state, rewritten constantly.
// Counting it would report a fresh crash every day, forever.
func TestIsCrashFile(t *testing.T) {
	crashes := []string{"abc.dmp", "abc.dmp.log", "ABC.DMP"}
	notCrashes := []string{".RateLimit.json", "readme.txt", "abc.json"}

	for _, name := range crashes {
		if !isCrashFile(name) {
			t.Errorf("%q should count as a crash file", name)
		}
	}
	for _, name := range notCrashes {
		if isCrashFile(name) {
			t.Errorf("%q must NOT count as a crash file", name)
		}
	}
}

// Only .dmp.log has readable content; a bare .dmp is a binary minidump.
func TestHasReadableLog(t *testing.T) {
	if !hasReadableLog("x.dmp.log") {
		t.Error(".dmp.log should be readable")
	}
	if hasReadableLog("x.dmp") {
		t.Error("a binary minidump must not be offered as readable text")
	}
}

// Observed crash logs run to 6 MB. Only the tail is useful, and only the tail
// should be read.
func TestCrashExcerptReadsTailOfLargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.dmp.log")

	var sb strings.Builder
	for i := range 200000 {
		sb.WriteString("filler line to pad the file out to something large\n")
		_ = i
	}
	sb.WriteString("X-Plex-Token=leakedsecretvalue123\n")
	sb.WriteString("FATAL: the last line before it died\n")

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got := crashExcerpt(path)

	if !strings.Contains(got, "FATAL: the last line before it died") {
		t.Error("excerpt does not contain the end of the file")
	}
	if strings.Contains(got, "leakedsecretvalue123") {
		t.Error("excerpt leaked a token")
	}
	if lines := strings.Count(got, "\n") + 1; lines > crashExcerptLines {
		t.Errorf("excerpt has %d lines, want at most %d", lines, crashExcerptLines)
	}
}

func TestCrashExcerptMissingFile(t *testing.T) {
	if got := crashExcerpt(filepath.Join(t.TempDir(), "nope.dmp.log")); got != "" {
		t.Errorf("got %q, want empty for a missing file", got)
	}
}
