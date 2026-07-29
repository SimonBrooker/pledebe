package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// minManualInterval throttles repeated manual runs. The page has no
// authentication, so a POST endpoint that reads an entire database is worth
// rate-limiting even on a LAN.
const minManualInterval = 60 * time.Second

// deepRunner tracks a manually triggered deep check.
//
// Exactly one runs at a time. A second request while one is in flight is
// ignored rather than queued — the user wants a current answer, and they are
// about to get one.
type deepRunner struct {
	run func(context.Context) error

	mu        sync.Mutex
	running   bool
	startedAt time.Time
	endedAt   time.Time
	lastErr   error
}

func (r *deepRunner) status() (running bool, startedAt time.Time, lastErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, r.startedAt, r.lastErr
}

// start kicks off a run in the background and reports whether it began.
func (r *deepRunner) start() bool {
	r.mu.Lock()
	if r.running || (!r.endedAt.IsZero() && time.Since(r.endedAt) < minManualInterval) {
		r.mu.Unlock()
		return false
	}
	r.running = true
	r.startedAt = time.Now()
	r.lastErr = nil
	r.mu.Unlock()

	go func() {
		// Detached from the request: the user's browser is redirected
		// immediately and a long check must not be cancelled by them
		// navigating away.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()

		err := r.run(ctx)

		r.mu.Lock()
		r.running = false
		r.endedAt = time.Now()
		r.lastErr = err
		r.mu.Unlock()

		if err != nil {
			log.Printf("manual deep check failed: %v", err)
		}
	}()
	return true
}

// postDeepCheck handles the "Run deep check now" button.
//
// Plain form POST, then redirect — no JavaScript, and the result is not tied to
// the response, so a slow check does not hang the browser.
func (s *Server) postDeepCheck(w http.ResponseWriter, req *http.Request) {
	if s.runner == nil || s.runner.run == nil {
		http.Error(w, "deep checks are not available in this mode", http.StatusNotImplemented)
		return
	}
	if origin, host, ok := sameOrigin(req); !ok {
		// Name both values. An opaque "refused" leaves the operator guessing,
		// and the commonest cause -- a reverse proxy rewriting Host -- is
		// invisible without seeing them side by side.
		log.Printf("deep check refused: Origin %q does not match host %q", origin, host)
		http.Error(w, fmt.Sprintf(
			"Refused: this request came from %q but the page is served as %q.\n\n"+
				"If pledebe is behind a reverse proxy, forward the original host "+
				"(X-Forwarded-Host) or set PLEDEBE_ORIGIN to the address you use "+
				"in the browser.", origin, host), http.StatusForbidden)
		return
	}

	s.runner.start()
	http.Redirect(w, req, "/", http.StatusSeeOther)
}

// sameOrigin rejects cross-site form submissions.
//
// The status page may have no authentication, so without this any web page the
// user visits could trigger work on their server.
//
// It returns the compared values so a refusal can explain itself rather than
// saying only "refused".
func sameOrigin(req *http.Request) (origin, host string, ok bool) {
	origin = req.Header.Get("Origin")

	// Browsers omit Origin on some same-origin form posts, and send the literal
	// "null" from sandboxed contexts. Neither indicates a cross-site request
	// here: this endpoint is reachable only from a private network, and the
	// alternative is refusing legitimate clicks.
	if origin == "" || origin == "null" {
		return origin, req.Host, true
	}

	u, err := url.Parse(origin)
	if err != nil {
		return origin, req.Host, false
	}

	// Behind a reverse proxy, Host is often rewritten to the internal upstream
	// while Origin stays the address the user typed. Compare against what the
	// proxy says the user asked for, when it says so.
	candidates := []string{req.Host}
	if fwd := req.Header.Get("X-Forwarded-Host"); fwd != "" {
		candidates = append(candidates, fwd)
	}
	if configured := os.Getenv("PLEDEBE_ORIGIN"); configured != "" {
		if cu, err := url.Parse(configured); err == nil && cu.Host != "" {
			candidates = append(candidates, cu.Host)
		} else {
			candidates = append(candidates, configured)
		}
	}

	for _, c := range candidates {
		if strings.EqualFold(u.Host, c) {
			return origin, req.Host, true
		}
	}
	return origin, req.Host, false
}

// deepStatus is what the template needs to render the button and its state.
type deepStatus struct {
	// Available gates the whole block. A zero struct is TRUTHY to Go
	// templates -- {{with}} and {{if}} only treat nil, false, 0 and
	// zero-length collections as empty -- so a struct field cannot be used as
	// its own presence check.
	Available bool

	Running   bool
	StartedAt time.Time
	Error     string

	// Estimate describes the expected cost, taken from the previous run where
	// one exists. A specific number is far more useful than a vague warning.
	Estimate string

	// Blocked is set when there is not enough free space to take a snapshot,
	// in which case the button is not offered at all.
	Blocked string
}

func (s *Server) deepStatus(lastRun *deepCheckCost) deepStatus {
	var ds deepStatus
	if s.runner == nil {
		return ds
	}
	ds.Available = true

	running, startedAt, err := s.runner.status()
	ds.Running = running
	ds.StartedAt = startedAt
	if err != nil {
		ds.Error = err.Error()
	}

	if lastRun != nil && lastRun.SnapshotSec > 0 {
		ds.Estimate = fmt.Sprintf("about %.0f seconds, based on the last run", lastRun.SnapshotSec+lastRun.CheckSec)
	}
	if lastRun != nil && lastRun.FreeBytes > 0 && lastRun.DatabaseBytes > 0 &&
		lastRun.FreeBytes < lastRun.DatabaseBytes+lastRun.DatabaseBytes/5 {
		ds.Blocked = fmt.Sprintf("needs about %s free for the snapshot, and only %s is available",
			humanBytes(lastRun.DatabaseBytes+lastRun.DatabaseBytes/5), humanBytes(lastRun.FreeBytes))
	}
	return ds
}

// deepCheckCost carries what the estimate and the space gate need.
type deepCheckCost struct {
	SnapshotSec   float64
	CheckSec      float64
	DatabaseBytes int64
	FreeBytes     int64
}
