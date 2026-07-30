package web

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/SimonBrooker/pledebe/internal/notify"
)

// testMailInterval throttles the test button. Sending is a side effect on
// someone else's mail server, and a button that can be held down is a way to
// get an IP blocked.
const testMailInterval = 30 * time.Second

// mailTester tracks the outcome of the last test send, so the page can report
// it without JavaScript.
type mailTester struct {
	cfg notify.Config

	mu       sync.Mutex
	at       time.Time
	ok       bool
	message  string
	attempts int
}

// envVar reports whether a setting is present, and its value when that value is
// not secret.
//
// SMTP_PASSWORD is shown only as "set" — never its value. The page may be
// reachable without authentication, and a mail password is often a real mailbox
// credential.
type envVar struct {
	Name     string
	Set      bool
	Value    string
	Secret   bool
	Required bool
}

// emailStatus is what the bell panel renders.
type emailStatus struct {
	Configured bool
	Vars       []envVar

	// Missing names the required settings that are absent, so the panel can say
	// what to add rather than only that something is wrong.
	Missing []string

	CanTest    bool
	LastTestAt time.Time
	LastTestOK bool
	LastTest   string
}

func (t *mailTester) status() emailStatus {
	c := t.cfg

	s := emailStatus{Configured: c.Enabled()}
	s.Vars = []envVar{
		{Name: "SMTP_HOST", Set: c.Host != "", Value: c.Host, Required: true},
		{Name: "SMTP_PORT", Set: c.Port != 0, Value: fmt.Sprint(c.Port)},
		{Name: "SMTP_USER", Set: c.User != "", Value: c.User},
		{Name: "SMTP_PASSWORD", Set: c.Password != "", Secret: true},
		{Name: "SMTP_FROM", Set: c.From != "", Value: c.From, Required: true},
		{Name: "SMTP_TO", Set: len(c.To) > 0, Value: joinTo(c.To), Required: true},
	}
	for _, v := range s.Vars {
		if v.Required && !v.Set {
			s.Missing = append(s.Missing, v.Name)
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	s.LastTestAt, s.LastTestOK, s.LastTest = t.at, t.ok, t.message
	s.CanTest = c.Enabled() && time.Since(t.at) > testMailInterval

	return s
}

func joinTo(to []string) string {
	switch len(to) {
	case 0:
		return ""
	case 1:
		return to[0]
	default:
		return fmt.Sprintf("%s and %d more", to[0], len(to)-1)
	}
}

// send delivers a test message and records the outcome.
func (t *mailTester) send(host string) {
	t.mu.Lock()
	if time.Since(t.at) < testMailInterval {
		t.mu.Unlock()
		return
	}
	t.attempts++
	n := t.attempts
	t.mu.Unlock()

	subject := fmt.Sprintf("pledebe (%s): test message", host)
	body := fmt.Sprintf(
		"This is a test message from pledebe on %s.\n\n"+
			"If you received it, notification is working: you will be emailed when a\n"+
			"problem appears and again when it clears. pledebe does not email about\n"+
			"things it could not measure, and never repairs anything itself.\n\n"+
			"Test %d.\n", host, n)

	err := t.cfg.Send(subject, body)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.at = time.Now()
	if err != nil {
		t.ok = false
		// The SMTP error is the useful part -- "authentication failed" and
		// "connection refused" need different fixes -- and it contains no
		// credentials.
		t.message = err.Error()
		log.Printf("test email failed: %v", err)
		return
	}
	t.ok = true
	t.message = "Sent. Check the inbox for " + joinTo(t.cfg.To) + "."
	log.Printf("test email sent to %s", joinTo(t.cfg.To))
}

// postTestEmail handles the button in the bell panel.
func (s *Server) postTestEmail(w http.ResponseWriter, req *http.Request) {
	if s.mail == nil || !s.mail.cfg.Enabled() {
		http.Error(w, "email is not configured", http.StatusPreconditionFailed)
		return
	}
	if origin, host, ok := sameOrigin(req); !ok {
		log.Printf("test email refused: Origin %q does not match host %q", origin, host)
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}

	// Synchronous: a test whose result you have to refresh to discover is worth
	// little, and SMTP either answers in a few seconds or has failed.
	s.mail.send(s.NotifyHost)

	http.Redirect(w, req, "/", http.StatusSeeOther)
}
