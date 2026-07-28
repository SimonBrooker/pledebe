// Package web serves the status page.
//
// Server-rendered HTML with no JavaScript build step: the page is a status
// readout and a history table, and a bundler would be the largest maintenance
// burden in the project for no benefit. It refreshes with a meta tag.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"github.com/SimonBrooker/pledebe/internal/health"
	"github.com/SimonBrooker/pledebe/internal/plex"
	"github.com/SimonBrooker/pledebe/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server renders the status page from stored history.
type Server struct {
	Install *plex.Install
	Store   *store.Store

	tmpl *template.Template
}

type pageData struct {
	Install     *plex.Install
	Metrics     *plex.Metrics
	Findings    []health.Finding
	History     []*plex.Metrics
	SampleCount int
	FreePercent float64
}

// New builds a server, parsing templates up front so a template error surfaces
// at startup rather than on the first request.
func New(install *plex.Install, st *store.Store) (*Server, error) {
	funcs := template.FuncMap{"bytes": humanBytes}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{Install: install, Store: st, tmpl: tmpl}, nil
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.status)
	mux.HandleFunc("GET /api/latest", s.apiLatest)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
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

	history, err := s.Store.Recent(50)
	if err != nil {
		http.Error(w, "reading history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	count, _ := s.Store.Count()

	data := pageData{
		Install:     s.Install,
		Metrics:     latest,
		Findings:    health.Evaluate(latest),
		History:     history,
		SampleCount: count,
		FreePercent: latest.FreeRatio() * 100,
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
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"metrics":  latest,
		"findings": health.Evaluate(latest),
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
