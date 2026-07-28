package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SimonBrooker/pledebe/internal/plex"
	"github.com/SimonBrooker/pledebe/internal/store"
)

func serverWithAuth(t *testing.T, auth Auth) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Insert(fullMetrics()); err != nil {
		t.Fatal(err)
	}
	srv, err := New(&plex.Install{Database: "/db"}, "/plexbin/Plex SQLite", "test", st,
		func(ctx context.Context) error { return nil }, auth)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func TestAuthRequiredWhenConfigured(t *testing.T) {
	h := serverWithAuth(t, Auth{User: "admin", Password: "correct-horse"})

	for _, path := range []string{"/", "/api/latest"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials = %d, want 401", path, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s did not challenge for basic auth", path)
		}
	}

	// The button endpoint is the one that makes the server do work.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/deepcheck", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /deepcheck without credentials = %d, want 401", rec.Code)
	}
}

func TestAuthAcceptsCorrectCredentials(t *testing.T) {
	h := serverWithAuth(t, Auth{User: "admin", Password: "correct-horse"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "correct-horse")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestAuthRejectsWrongPassword(t *testing.T) {
	h := serverWithAuth(t, Auth{User: "admin", Password: "correct-horse"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// Container health checks must not need credentials, and it reveals nothing.
func TestHealthzIsUnauthenticated(t *testing.T) {
	h := serverWithAuth(t, Auth{User: "admin", Password: "correct-horse"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200", rec.Code)
	}
}

func TestSecurityHeadersAlwaysSet(t *testing.T) {
	h := serverWithAuth(t, Auth{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	// The page has no scripts and loads nothing externally, so the policy can
	// be this tight. A future change that needs a script should be a deliberate
	// decision, not a silent relaxation.
	for _, directive := range []string{"default-src 'none'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q; got %q", directive, csp)
		}
	}
	if strings.Contains(csp, "script-src") {
		t.Error("CSP grants script permissions; the page uses no JavaScript")
	}
}

// Credentials must never appear in the page or the API.
func TestNoSecretsInOutput(t *testing.T) {
	h := serverWithAuth(t, Auth{User: "admin", Password: "correct-horse"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "correct-horse")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	for _, secret := range []string{"correct-horse", "PlexOnlineToken"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("response contains %q", secret)
		}
	}
}
