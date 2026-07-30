package notify

import (
	"fmt"
	"strings"

	"github.com/SimonBrooker/pledebe/internal/health"
)

// Change describes what to tell the operator about since the last check.
type Change struct {
	New       []health.Finding
	Recovered []string
}

// Any reports whether there is anything worth sending.
func (c Change) Any() bool { return len(c.New) > 0 || len(c.Recovered) > 0 }

// Diff compares current findings against those already notified.
//
// Only Warn and Fault are considered. Unknown is deliberately excluded: it means
// pledebe could not measure something, which is a gap in its own knowledge
// rather than a problem with the server, and emailing about it would train the
// reader to ignore the alerts that matter.
func Diff(findings []health.Finding, alreadyNotified map[string]string) Change {
	var change Change
	current := map[string]bool{}

	for _, f := range findings {
		if f.Level != health.LevelWarn && f.Level != health.LevelFault {
			continue
		}
		current[f.Title] = true
		if _, known := alreadyNotified[f.Title]; !known {
			change.New = append(change.New, f)
		}
	}

	for title := range alreadyNotified {
		if !current[title] {
			change.Recovered = append(change.Recovered, title)
		}
	}
	return change
}

// Subject summarises a change for the mail header.
//
// Leads with the host so someone running pledebe on several servers can tell
// them apart from the inbox list alone.
func Subject(host string, c Change) string {
	switch {
	case len(c.New) == 1:
		return fmt.Sprintf("pledebe (%s): %s", host, c.New[0].Title)
	case len(c.New) > 1:
		return fmt.Sprintf("pledebe (%s): %d problems found", host, len(c.New))
	case len(c.Recovered) == 1:
		return fmt.Sprintf("pledebe (%s): resolved — %s", host, c.Recovered[0])
	case len(c.Recovered) > 1:
		return fmt.Sprintf("pledebe (%s): %d problems resolved", host, len(c.Recovered))
	}
	return fmt.Sprintf("pledebe (%s)", host)
}

// Body writes the message.
//
// Plain text, and the detail of each finding verbatim — those strings were
// written to explain themselves on the status page, and repeating them here
// means the email is actionable without opening anything.
func Body(host string, c Change, baseURL string) string {
	var b strings.Builder

	if len(c.New) > 0 {
		if len(c.New) == 1 {
			b.WriteString("pledebe found a problem with the Plex database on " + host + ".\n\n")
		} else {
			fmt.Fprintf(&b, "pledebe found %d problems with the Plex database on %s.\n\n",
				len(c.New), host)
		}
		for _, f := range c.New {
			label := "ATTENTION"
			if f.Level == health.LevelFault {
				label = "FAULT"
			}
			fmt.Fprintf(&b, "[%s] %s\n%s\n\n", label, f.Title, f.Detail)
		}
	}

	if len(c.Recovered) > 0 {
		b.WriteString("No longer reported:\n")
		for _, title := range c.Recovered {
			fmt.Fprintf(&b, "  - %s\n", title)
		}
		b.WriteString("\n")
	}

	if baseURL != "" {
		fmt.Fprintf(&b, "Status page: %s\n\n", baseURL)
	}

	// Say plainly what pledebe will and will not do next, so nobody waits for a
	// second warning that is never coming.
	b.WriteString("You will not be emailed about these again unless they clear and " +
		"return. pledebe never repairs anything itself.\n")

	return b.String()
}
