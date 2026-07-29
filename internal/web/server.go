// Package web serves the status page.
//
// Server-rendered HTML with no JavaScript build step, and no JavaScript at all:
// the page is a status readout, the "more history" disclosure is a <details>
// element, and it refreshes with a meta tag. It therefore prints, works in any
// browser, and cannot break in a way that hides a finding.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SimonBrooker/pledebe/internal/health"
	"github.com/SimonBrooker/pledebe/internal/plex"
	"github.com/SimonBrooker/pledebe/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// recentDays is how many daily rows appear before the "more" disclosure.
const recentDays = 7

// historyDepth bounds how much history the page renders at all.
const historyDepth = 90

// Server renders the status page from stored history.
type Server struct {
	Install    *plex.Install
	SQLitePath string
	Version    string
	Store      *store.Store
	Auth       Auth

	runner *deepRunner
	tmpl   *template.Template
}

type pageData struct {
	Install    *plex.Install
	SQLitePath string
	Version    string
	VersionURL string

	Metrics   *plex.Metrics
	Deep      *plex.DeepCheck
	Summary   health.Summary
	Findings  []health.Finding
	Recommend *health.Recommendation

	RecentDays  []store.DailySample
	OlderDays   []store.DailySample
	DayCount    int
	SampleCount int

	FreePercent  float64
	ButlerWindow string
	Levels       map[string]health.Level
	DeepRun      deepStatus
}

// New builds a server, parsing templates up front so a template error surfaces
// at startup rather than on the first request.
// New builds a server. runDeep may be nil, in which case the page offers no
// button — a deployment that cannot run checks should not pretend it can.
func New(install *plex.Install, sqlitePath, version string, st *store.Store,
	runDeep func(context.Context) error, auth Auth) (*Server, error) {
	funcs := template.FuncMap{
		"bytes": humanBytes,
		// Accepts any integer width: the metrics mix int and int64, and a
		// template type mismatch is a runtime failure, not a compile one.
		"num": numAny,
		"ago": ago,
		"pct": func(part, whole int64) string { return percent(part, whole) },
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	srv := &Server{
		Install: install, SQLitePath: sqlitePath, Version: version,
		Store: st, tmpl: tmpl, Auth: auth,
	}
	if runDeep != nil {
		srv.runner = &deepRunner{run: runDeep}
	}
	return srv, nil
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.requireAuth(s.status))
	mux.HandleFunc("POST /deepcheck", s.requireAuth(s.postDeepCheck))
	mux.HandleFunc("GET /api/latest", s.requireAuth(s.apiLatest))

	// Unauthenticated on purpose: container health checks must reach it, and
	// it reveals nothing beyond the process being alive.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return secureHeaders(mux)
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	latest, err := s.Store.Latest()
	if err != nil {
		http.Error(w, "reading history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if latest == nil {
		http.Error(w, "no samples collected yet — check back shortly", http.StatusServiceUnavailable)
		return
	}

	deep, _ := s.Store.LatestDeepCheck()
	findings := health.Evaluate(latest, deep)

	days, err := s.Store.RecentDays(historyDepth)
	if err != nil {
		http.Error(w, "reading history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sampleCount, _ := s.Store.Count()
	dayCount, _ := s.Store.DayCount()

	data := pageData{
		Install:      s.Install,
		SQLitePath:   s.SQLitePath,
		Version:      s.Version,
		VersionURL:   releaseURL(s.Version),
		Metrics:      latest,
		Deep:         deep,
		Summary:      health.Summarise(findings),
		Findings:     findings,
		Recommend:    health.Recommend(latest, deep),
		DayCount:     dayCount,
		SampleCount:  sampleCount,
		FreePercent:  latest.FreeRatio() * 100,
		Levels:       health.MetricLevels(latest, deep),
		ButlerWindow: butlerWindowText(latest),
	}

	cost := &deepCheckCost{
		DatabaseBytes: latest.DatabaseBytes,
		FreeBytes:     latest.VolumeFreeBytes,
	}
	if deep != nil {
		cost.SnapshotSec, cost.CheckSec = deep.SnapshotSec, deep.CheckSec
	}
	data.DeepRun = s.deepStatus(cost)
	if len(days) > recentDays {
		data.RecentDays, data.OlderDays = days[:recentDays], days[recentDays:]
	} else {
		data.RecentDays = days
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "status.html", data); err != nil {
		// Too late for an error page; the response is already partly written.
		fmt.Fprintf(w, "\n<!-- template error: %v -->", err)
	}
}

func (s *Server) apiLatest(w http.ResponseWriter, _ *http.Request) {
	latest, err := s.Store.Latest()
	if err != nil || latest == nil {
		http.Error(w, "no samples yet", http.StatusServiceUnavailable)
		return
	}
	deep, _ := s.Store.LatestDeepCheck()
	findings := health.Evaluate(latest, deep)

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"summary":    health.Summarise(findings),
		"findings":   findings,
		"metrics":    latest,
		"deep_check": deep,
	})
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// numAny formats any integer type with digit grouping. html/template resolves
// argument types at execution time, so a func(int64) silently fails on an int
// field — caught only when the page is actually rendered.
func numAny(v any) string {
	switch n := v.(type) {
	case int:
		return comma(int64(n))
	case int32:
		return comma(int64(n))
	case int64:
		return comma(n)
	case uint:
		return comma(int64(n))
	case uint64:
		return comma(int64(n))
	default:
		return fmt.Sprint(v)
	}
}

// comma groups digits. Page counts and document counts run to seven figures,
// where unseparated digits are genuinely hard to compare between rows.
func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	var out strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(r)
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

func percent(part, whole int64) string {
	if whole == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(part)/float64(whole)*100)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// butlerWindowText names the server's actual maintenance hours, read from
// Preferences.xml at collection time. Falling back to PMS defaults would tell a
// reader who changed the window something untrue.
// It returns an empty string when there is no window to describe, so the
// template can drop the sentence rather than assert something untrue.
func butlerWindowText(m *plex.Metrics) string {
	if m == nil || m.SlowQueries == nil {
		return ""
	}
	start, end := m.SlowQueries.ButlerStart, m.SlowQueries.ButlerEnd

	// Equal hours mean the window is empty — the counting logic already treats
	// it that way, so claiming a window here would contradict the figures.
	if start == end {
		return ""
	}

	window := fmt.Sprintf("%02d:00–%02d:00", start, end)

	// We could not read Preferences.xml, so these are Plex's defaults rather
	// than this server's settings. Say which, instead of presenting a guess as
	// a fact — the same rule the findings follow.
	if m.PreferencesNote != "" {
		return window + " (assumed — pledebe cannot read your Plex settings)"
	}
	return window
}

// releaseURL links the displayed version to its GitHub release.
//
// A released build links to its own release notes; a development build links to
// the releases list, since there is no page for an untagged commit.
func releaseURL(version string) string {
	const base = "https://github.com/SimonBrooker/pledebe/releases"

	v := strings.TrimPrefix(version, "v")
	if v == "" || !isSemver(v) {
		return base
	}
	return base + "/tag/v" + v
}

// isSemver reports whether v looks like MAJOR.MINOR.PATCH. Deliberately strict:
// linking a commit SHA to a non-existent release page would be worse than
// linking to the list.
func isSemver(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}
