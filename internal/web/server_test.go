package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SimonBrooker/pledebe/internal/notify"
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
	srv, err := New(install, "/plexbin/Plex SQLite", "test", st, nil, Auth{}, notify.Config{}, "testhost")
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
	srv, err := New(&plex.Install{Database: "/db"}, "/plexbin/Plex SQLite", "test", st, run, Auth{}, notify.Config{}, "testhost")
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

// A same-origin click must never be refused. Browsers vary in whether they send
// Origin on same-origin form posts, and sandboxed contexts send the literal
// "null" -- refusing either turns a working button into an opaque error.
func TestSameOriginAcceptsRealBrowserBehaviour(t *testing.T) {
	cases := map[string]string{
		"no Origin header": "",
		"literal null":     "null",
		"matching Origin":  "http://10.20.30.13:8087",
		"different case":   "http://10.20.30.13:8087",
	}

	for name, origin := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/deepcheck", nil)
			req.Host = "10.20.30.13:8087"
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			if _, _, ok := sameOrigin(req); !ok {
				t.Errorf("refused a legitimate request with Origin %q", origin)
			}
		})
	}
}

// A reverse proxy commonly rewrites Host to the internal upstream while Origin
// stays the address the user typed.
func TestSameOriginTrustsForwardedHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/deepcheck", nil)
	req.Host = "pledebe:8080" // what the proxy passed upstream
	req.Header.Set("Origin", "https://plex-health.example.com")
	req.Header.Set("X-Forwarded-Host", "plex-health.example.com")

	if _, _, ok := sameOrigin(req); !ok {
		t.Error("refused a proxied request that forwarded the original host")
	}
}

func TestSameOriginStillRefusesGenuineCrossSite(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/deepcheck", nil)
	req.Host = "10.20.30.13:8087"
	req.Header.Set("Origin", "https://evil.example")

	if _, _, ok := sameOrigin(req); ok {
		t.Error("accepted a genuine cross-site request")
	}
}

// The maintenance window shown next to slow queries comes from the server's own
// Plex settings. It must never present Plex's defaults as if they were read.
func TestButlerWindowText(t *testing.T) {
	cases := []struct {
		name string
		m    *plex.Metrics
		want string
	}{
		{
			name: "read from the server's settings",
			m: &plex.Metrics{SlowQueries: &plex.SlowQueries{
				ButlerStart: 3, ButlerEnd: 9,
			}},
			want: "03:00–09:00",
		},
		{
			name: "unreadable preferences are marked as assumed",
			m: &plex.Metrics{
				PreferencesNote: "cannot read Preferences.xml: owned by uid 1000",
				SlowQueries:     &plex.SlowQueries{ButlerStart: 2, ButlerEnd: 8},
			},
			want: "02:00–08:00 (assumed — pledebe cannot read your Plex settings)",
		},
		{
			// Equal hours mean no window. Claiming one would contradict the
			// "during maintenance" count, which is zero in that case.
			name: "no window at all",
			m: &plex.Metrics{SlowQueries: &plex.SlowQueries{
				ButlerStart: 4, ButlerEnd: 4,
			}},
			want: "",
		},
		{
			name: "no slow queries collected",
			m:    &plex.Metrics{},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := butlerWindowText(tc.m); got != tc.want {
				t.Errorf("butlerWindowText = %q, want %q", got, tc.want)
			}
		})
	}
}

// A quiet interval is a result, not an absence of one. Returning nil made the
// whole panel vanish whenever nothing was slow for fifteen minutes, which reads
// as the feature having broken rather than as good news.
func TestSlowQuerySectionSurvivesAQuietInterval(t *testing.T) {
	m := fullMetrics()
	m.SlowQueries = &plex.SlowQueries{LogsReadable: true, ButlerStart: 2, ButlerEnd: 8}

	body := render(t, newTestServer(t, m, nil))

	if !strings.Contains(body, "Slow queries") {
		t.Error("the section disappeared when there were none")
	}
	if !strings.Contains(body, "no slow queries since the last check") {
		t.Error("a quiet interval should say so rather than showing nothing")
	}
}

// nil means the logs could not be read, which is unmeasured -- not a report of
// zero.
func TestSlowQuerySectionSaysWhenLogsUnreadable(t *testing.T) {
	m := fullMetrics()
	m.SlowQueries = nil

	body := render(t, newTestServer(t, m, nil))

	if !strings.Contains(body, "cannot read Plex's logs") {
		t.Error("unreadable logs must be reported as unmeasured, not as zero")
	}
	if strings.Contains(body, "no slow queries since the last check") {
		t.Error("unreadable logs must not be reported as a quiet interval")
	}
}

func TestSlowQuerySectionShowsFigures(t *testing.T) {
	m := fullMetrics()
	m.SlowQueries = &plex.SlowQueries{
		LogsReadable: true, Count: 725, P50: 230, P95: 460, Max: 1200,
		InButlerWindow: 700, ButlerStart: 2, ButlerEnd: 8,
		Since: time.Now().Add(-6 * time.Hour), Until: time.Now(),
	}

	body := render(t, newTestServer(t, m, nil))
	for _, want := range []string{"725", "230 ms", "460 ms", "700"} {
		if !strings.Contains(body, want) {
			t.Errorf("figures missing %q", want)
		}
	}
}

// Icons and the manifest must be reachable without credentials: a browser
// fetches /favicon.ico before it has any to offer, and gating it would pop a
// basic-auth prompt for an icon.
func TestIconsAndManifestUnauthenticated(t *testing.T) {
	h := serverWithAuth(t, Auth{User: "admin", Password: "correct-horse"})

	for _, path := range []string{
		"/favicon.ico",
		"/manifest.webmanifest",
		"/static/icon-32.png",
		"/static/icon-192.png",
		"/static/icon-512.png",
		"/static/icon-180.png",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s served no bytes", path)
		}
	}
}

func TestManifestContentTypeAndContents(t *testing.T) {
	h := newTestServer(t, fullMetrics(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))

	// http.FileServer cannot infer this from the extension, so it is set
	// explicitly; without it browsers ignore the manifest.
	if got := rec.Header().Get("Content-Type"); got != "application/manifest+json" {
		t.Errorf("Content-Type = %q, want application/manifest+json", got)
	}
	// Chrome needs a 192 and a 512 icon to consider the app installable.
	for _, want := range []string{"icon-192.png", "icon-512.png", "standalone"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("manifest missing %q", want)
		}
	}
}

// The page is no-store, but assets change only when the binary does.
func TestStaticAssetsAreCacheable(t *testing.T) {
	h := newTestServer(t, fullMetrics(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/icon-32.png", nil))

	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
		t.Errorf("Cache-Control = %q, want a long max-age", got)
	}
}

func TestPageLinksIconAndManifest(t *testing.T) {
	body := render(t, newTestServer(t, fullMetrics(), nil))

	for _, want := range []string{
		`rel="icon"`,
		`rel="apple-touch-icon"`,
		`rel="manifest"`,
		`name="theme-color"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page head missing %s", want)
		}
	}
}

// The CSP must permit the icon and manifest it now references, and must still
// grant nothing for scripts.
func TestCSPAllowsIconsButNotScripts(t *testing.T) {
	h := newTestServer(t, fullMetrics(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"img-src 'self'", "manifest-src 'self'", "default-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
	if strings.Contains(csp, "script-src") {
		t.Error("CSP grants script permissions; the page uses no JavaScript")
	}
}

// The bell reports whether notification is configured, and must never render
// the password itself — the page may be reachable without authentication.
func TestBellShowsStatusWithoutLeakingPassword(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Insert(fullMetrics()); err != nil {
		t.Fatal(err)
	}

	cfg := notify.Config{
		Host: "smtp.example", Port: 587,
		User: "someone@example", Password: "hunter2-should-never-appear",
		From: "someone@example", To: []string{"someone@example"},
	}
	srv, err := New(&plex.Install{Database: "/db"}, "/plexbin/Plex SQLite", "test",
		st, nil, Auth{}, cfg, "buzz")
	if err != nil {
		t.Fatal(err)
	}

	body := render(t, srv.Handler())

	if strings.Contains(body, "hunter2-should-never-appear") {
		t.Fatal("the SMTP password was rendered into the page")
	}
	for _, want := range []string{"Send test email", "SMTP_HOST", "smtp.example"} {
		if !strings.Contains(body, want) {
			t.Errorf("bell panel missing %q", want)
		}
	}
}

// Unconfigured, the bell says which required variables are absent rather than
// only that something is wrong.
func TestBellNamesMissingVariables(t *testing.T) {
	body := render(t, newTestServer(t, fullMetrics(), nil))

	if !strings.Contains(body, "Email notification is off") {
		t.Error("expected the bell to report notification as off")
	}
	for _, want := range []string{"SMTP_HOST", "SMTP_FROM", "SMTP_TO"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing-variable list should name %q", want)
		}
	}
	if strings.Contains(body, "Send test email") {
		t.Error("offered a test button with nothing configured")
	}
}

// Sending mail is a side effect; the endpoint needs the same protections as the
// deep-check button.
func TestTestEmailEndpointGuards(t *testing.T) {
	h := newTestServer(t, fullMetrics(), nil) // no mail configured

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/test-email", nil))
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("unconfigured = %d, want 412", rec.Code)
	}

	// Cross-origin must be refused even when it is configured.
	st, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Insert(fullMetrics()); err != nil {
		t.Fatal(err)
	}
	cfg := notify.Config{Host: "smtp.invalid", From: "a@b", To: []string{"c@d"}}
	srv, err := New(&plex.Install{Database: "/db"}, "/x", "test", st, nil, Auth{}, cfg, "buzz")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/test-email", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin = %d, want 403", rec.Code)
	}
}
