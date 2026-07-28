package web

import (
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
	srv, err := New(install, "/plexbin/Plex SQLite", "test", st)
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
