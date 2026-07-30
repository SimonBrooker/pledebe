package notify

import (
	"strings"
	"testing"

	"github.com/SimonBrooker/pledebe/internal/health"
)

// The whole point of the feature: a corrupt database must produce ONE email,
// not one every fifteen minutes.
func TestDiffOnlyReportsNewFindings(t *testing.T) {
	findings := []health.Finding{
		{Level: health.LevelFault, Title: "Database integrity check FAILED"},
		{Level: health.LevelWarn, Title: "Search indexes report corruption"},
	}

	first := Diff(findings, map[string]string{})
	if len(first.New) != 2 {
		t.Fatalf("first check reported %d, want 2", len(first.New))
	}

	// Second poll, same problems, already notified.
	already := map[string]string{
		"Database integrity check FAILED":  "fault",
		"Search indexes report corruption": "warn",
	}
	second := Diff(findings, already)
	if second.Any() {
		t.Errorf("re-reported known findings: %+v", second)
	}
}

// Unknown means pledebe could not measure something. Emailing about a gap in
// its own knowledge would train the reader to ignore the alerts that matter.
func TestDiffIgnoresUnknownAndOK(t *testing.T) {
	findings := []health.Finding{
		{Level: health.LevelUnknown, Title: "Backup freshness unknown"},
		{Level: health.LevelUnknown, Title: "Cannot read Plex's settings"},
		{Level: health.LevelOK, Title: "Database integrity verified"},
	}

	if c := Diff(findings, map[string]string{}); c.Any() {
		t.Errorf("notified about non-problems: %+v", c)
	}
}

func TestDiffReportsRecovery(t *testing.T) {
	already := map[string]string{"Search indexes report corruption": "warn"}

	// The problem is gone from the current findings.
	c := Diff([]health.Finding{
		{Level: health.LevelOK, Title: "Search indexes healthy"},
	}, already)

	if len(c.Recovered) != 1 || c.Recovered[0] != "Search indexes report corruption" {
		t.Errorf("Recovered = %v, want the cleared finding", c.Recovered)
	}
}

// Subject leads with the host so several servers are distinguishable from the
// inbox list alone.
func TestSubject(t *testing.T) {
	one := Change{New: []health.Finding{{Title: "Database integrity check FAILED"}}}
	if got := Subject("buzz", one); got != "pledebe (buzz): Database integrity check FAILED" {
		t.Errorf("got %q", got)
	}

	two := Change{New: []health.Finding{{Title: "a"}, {Title: "b"}}}
	if got := Subject("buzz", two); !strings.Contains(got, "2 problems") {
		t.Errorf("got %q, want a count", got)
	}

	rec := Change{Recovered: []string{"Search indexes report corruption"}}
	if got := Subject("buzz", rec); !strings.Contains(got, "resolved") {
		t.Errorf("got %q, want it to read as good news", got)
	}
}

// The body repeats each finding's detail verbatim, so the email is actionable
// without opening the status page.
func TestBodyCarriesDetailAndExpectations(t *testing.T) {
	c := Change{New: []health.Finding{{
		Level:  health.LevelWarn,
		Title:  "Search indexes report corruption",
		Detail: "DBRepair's Reindex rebuilds them.",
	}}}

	body := Body("buzz", c, "http://buzz:8080")

	for _, want := range []string{
		"buzz",
		"Search indexes report corruption",
		"DBRepair's Reindex rebuilds them.",
		"http://buzz:8080",
		"not be emailed about these again", // so nobody waits for a second warning
		"never repairs anything itself",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// Finding titles include values read from the Plex database and its logs. A
// newline in a header would let that inject arbitrary headers or a second body.
func TestHeaderInjectionIsStripped(t *testing.T) {
	cfg := Config{
		Host: "smtp.example", From: "a@example", To: []string{"b@example"},
	}
	msg := string(cfg.message("evil\r\nBcc: attacker@example", "body"))

	// Substring matching is not enough here: the sanitised subject ends with
	// CRLF, so "Bcc: ...\r\n" appears as a substring of a legitimate Subject
	// line. What matters is whether it became a header in its own right.
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Errorf("header injection succeeded: %q became its own header", line)
		}
	}
	if !strings.Contains(msg, "Subject: evil  Bcc: attacker@example") {
		t.Errorf("expected the newline replaced, got:\n%s", msg)
	}
}

func TestEnabledAndValidate(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("empty config must be disabled")
	}
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("an unconfigured mailer is fine, got %v", err)
	}

	// Half-configured is a mistake worth failing at startup, not silently
	// never sending.
	partial := Config{Host: "smtp.example"}
	if err := partial.Validate(); err == nil {
		t.Error("expected an error naming what is missing")
	} else if !strings.Contains(err.Error(), "SMTP_FROM") {
		t.Errorf("error should name the missing setting, got %v", err)
	}

	full := Config{Host: "smtp.example", From: "a@example", To: []string{"b@example"}}
	if !full.Enabled() {
		t.Error("fully configured mailer should be enabled")
	}
	if err := full.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}
