package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/plex"
	"github.com/SimonBrooker/pledebe/internal/store"
)

func newTestServer(t *testing.T, m *plex.Metrics, dc *plex.DeepCheck) http.Handler {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if m != nil {
		if err := st.Insert(m); err != nil {
			t.Fatal(err)
		}
		if err := st.RollupDay(m.CollectedAt); err != nil {
			t.Fatal(err)
		}
	}
	if dc != nil {
		if err := st.InsertDeepCheck(dc); err != nil {
			t.Fatal(err)
		}
	}

	install := &plex.Install{
		ConfigRoot: "/plexconfig",
		Database:   "/plexconfig/Plug-in Support/Databases/com.plexapp.plugins.library.db",
		LogDir:     "/plexconfig/Logs",
	}
	srv, err := New(install, "/plexbin/Plex SQLite", "test", st, nil, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func fullMetrics() *plex.Metrics {
	return &plex.Metrics{
		CollectedAt:        time.Now().UTC(),
		DatabaseBytes:      1194328064,
		WALBytes:           4907008,
		SHMBytes:           32768,
		BlobsBytes:         3641327616,
		PageCount:          291757,
		PageSize:           4096,
		FreelistCount:      350,
		FreelistBytes:      1433600,
		NewestBackup:       "com.plexapp.plugins.library.db-2026-07-27",
		NewestBackupAt:     time.Now().UTC().Add(-24 * time.Hour),
		BackupCount:        4,
		BackupDirSearched:  "/plexbackups",
		BackupDirExpected:  "/backup/Databases",
		BackupDirVisible:   true,
		PMSVersion:         "1.43.3.10828-00f62d37d",
		VersionSeenAt:      time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		VolumeFreeBytes:    690835845120,
		CrashReportCount:   0,
		CrashesByComponent: map[string]int{"PLEX MEDIA SERVER": 0},
	}
}

func render(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The handler cannot send an error page once output has started, so it
	// appends a comment instead. Fail loudly on it -- this is what catches a
	// broken template expression, which otherwise ships silently.
	if strings.Contains(body, "template error:") {
		t.Fatalf("template failed to execute: %s", body[strings.Index(body, "template error:"):])
	}
	return body
}

func TestStatusPageRendersEverything(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt:        time.Now().UTC().Add(-10 * time.Minute),
		Duration:         11 * time.Second,
		SnapshotSec:      4,
		CheckSec:         6,
		DatabaseBytes:    1194328064,
		SnapshotBytes:    1179648000,
		ReclaimableBytes: 14680064,
		IntegrityOK:      true,
		FTS: []plex.FTSTable{
			{Name: "fts4_metadata_titles", IntegrityOK: false, IndexedDocs: 133995, SourceRows: 138181},
			{Name: "fts4_tag_titles_icu", IntegrityOK: false, IndexedDocs: 208818, SourceRows: 531055},
		},
	}

	body := render(t, newTestServer(t, fullMetrics(), dc))

	// Every group the page promises to show, checked by a value only it renders.
	for _, want := range []string{
		"291,757",                // page count, comma-formatted
		"4,096",                  // page size
		"Shared memory",          // was missing from the first mockup
		"Reclaimable (exact)",    // deep check
		"fts4_metadata_titles",   // FTS table, permanent
		"322,237",                // missing docs on the ICU tag index
		"/backup/Databases",      // PMS's configured path
		"/plexbackups",           // where we actually read
		"1.43.3.10828-00f62d37d", // Plex version
		"Plex SQLite",            // paths group
		"needs attention",        // verdict headline for a warning
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

// A cold start has no deep check. The page must still render, and must say the
// checks have not run rather than implying anything is wrong.
func TestStatusPageWithoutDeepCheck(t *testing.T) {
	body := render(t, newTestServer(t, fullMetrics(), nil))

	if !strings.Contains(body, "has not run yet") {
		t.Error("expected the page to say the deep check has not run")
	}
	if strings.Contains(body, "FAILED") {
		t.Error("a missing deep check must not render as a failure")
	}
}

// No samples at all is a 503 with an explanation, not a crash or a blank page.
func TestStatusPageBeforeFirstSample(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t, nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestAPIReturnsSummaryAndFindings(t *testing.T) {
	h := newTestServer(t, fullMetrics(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/latest", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{"summary", "findings", "metrics"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("API response missing %q", want)
		}
	}
}

func TestComma(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 999: "999", 1000: "1,000", 291757: "291,757",
		1338472: "1,338,472", -4186: "-4,186",
	} {
		if got := comma(in); got != want {
			t.Errorf("comma(%d) = %q, want %q", in, got, want)
		}
	}
}

func serverWithRunner(t *testing.T, run func(context.Context) error) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Insert(fullMetrics()); err != nil {
		t.Fatal(err)
	}
	srv, err := New(&plex.Install{Database: "/db"}, "/plexbin/Plex SQLite", "test", st, run, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func TestDeepCheckButtonAndWarningShown(t *testing.T) {
	body := render(t, serverWithRunner(t, func(context.Context) error { return nil }))

	if !strings.Contains(body, "Run deep check now") {
		t.Error("expected the run button")
	}
	// The warning must be specific about the cost, and equally specific that
	// Plex is not blocked -- an admin who thinks it stops playback will never
	// press it.
	for _, want := range []string{"heavy disk activity", "never blocked", "deleted afterwards"} {
		if !strings.Contains(body, want) {
			t.Errorf("warning is missing %q", want)
		}
	}
}

func TestDeepCheckPostStartsRun(t *testing.T) {
	started := make(chan struct{})
	h := serverWithRunner(t, func(context.Context) error {
		close(started)
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/deepcheck", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (post/redirect/get)", rec.Code)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the deep check was never started")
	}
}

// The page has no authentication, so any site the user visits could otherwise
// POST here and make their server do work.
func TestDeepCheckRefusesCrossOrigin(t *testing.T) {
	var called bool
	h := serverWithRunner(t, func(context.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodPost, "/deepcheck", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if called {
		t.Error("a cross-origin POST started a deep check")
	}
}

func TestDeepCheckAcceptsSameOrigin(t *testing.T) {
	h := serverWithRunner(t, func(context.Context) error { return nil })

	req := httptest.NewRequest(http.MethodPost, "/deepcheck", nil)
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
}

// Without a runner the page must not offer a button it cannot honour.
func TestDeepCheckUnavailableWithoutRunner(t *testing.T) {
	h := newTestServer(t, fullMetrics(), nil)

	if strings.Contains(render(t, h), "Run deep check now") {
		t.Error("button offered with no runner configured")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/deepcheck", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

// The action banner must appear only when DBRepair would actually help, and
// must carry the steps rather than just naming a problem.
func TestActionBannerShownForCorruptFTS(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt: time.Now(), IntegrityOK: true,
		FTS: []plex.FTSTable{{Name: "fts4_metadata_titles", IntegrityOK: false}},
	}
	body := render(t, newTestServer(t, fullMetrics(), dc))

	for _, want := range []string{"Action needed", "DBRepair: Reindex", "Stop Plex Media Server"} {
		if !strings.Contains(body, want) {
			t.Errorf("banner missing %q", want)
		}
	}
	if !strings.Contains(body, "github.com/ChuckPa/DBRepair") {
		t.Error("banner must link to DBRepair; pledebe does not repair anything itself")
	}
}

func TestNoActionBannerWhenHealthy(t *testing.T) {
	dc := &plex.DeepCheck{
		StartedAt: time.Now(), IntegrityOK: true,
		FTS: []plex.FTSTable{{Name: "fts4_metadata_titles", IntegrityOK: true}},
	}
	if strings.Contains(render(t, newTestServer(t, fullMetrics(), dc)), "Action needed") {
		t.Error("action banner shown on a healthy server")
	}
}

// The version in the top right links to its own release notes when the build is
// tagged, and to the releases list otherwise — linking a commit SHA to a
// non-existent release page would be worse than linking to the list.
func TestVersionLink(t *testing.T) {
	for version, want := range map[string]string{
		"1.0.0":         "https://github.com/SimonBrooker/pledebe/releases/tag/v1.0.0",
		"v1.2.3":        "https://github.com/SimonBrooker/pledebe/releases/tag/v1.2.3",
		"dev":           "https://github.com/SimonBrooker/pledebe/releases",
		"f0f623c8a91b2": "https://github.com/SimonBrooker/pledebe/releases",
		"":              "https://github.com/SimonBrooker/pledebe/releases",
	} {
		if got := releaseURL(version); got != want {
			t.Errorf("releaseURL(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestVersionRenderedInAppBar(t *testing.T) {
	body := render(t, newTestServer(t, fullMetrics(), nil))
	if !strings.Contains(body, `class="app-version"`) {
		t.Error("version link missing from the app bar")
	}
	if !strings.Contains(body, "github.com/SimonBrooker/pledebe/releases") {
		t.Error("version does not link to releases")
	}
}
