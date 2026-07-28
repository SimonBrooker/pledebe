package web

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"strings"
)

// Auth holds optional HTTP basic credentials.
//
// Off by default, because the common deployment is a NAS on a home LAN and
// forcing credentials on that user would mostly produce a shared password taped
// to the wall. But the page exposes filesystem paths, database geometry and a
// POST that makes the server read an entire database, so it should not be
// reachable from anywhere untrusted without it.
type Auth struct {
	User     string
	Password string
}

// Enabled reports whether credentials were configured.
func (a Auth) Enabled() bool { return a.User != "" && a.Password != "" }

// requireAuth wraps a handler with basic auth when credentials are set.
//
// Comparison is constant-time. A plain == leaks the length of the correct
// prefix through timing, which is exactly the kind of thing that is free to get
// right and awkward to retrofit.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !s.Auth.Enabled() {
			next(w, req)
			return
		}

		user, pass, ok := req.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.Auth.User)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.Auth.Password)) == 1

		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="pledebe", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, req)
	}
}

// secureHeaders applies defence-in-depth headers.
//
// The page is entirely self-contained — inline CSS, no scripts, no external
// resources — so the policy can be extremely tight. If a future change needs a
// script, that policy should be an explicit decision, not a silent relaxation.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// The page reflects the state of a private server; caches should not
		// keep it.
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, req)
	})
}

// WarnIfExposed logs a warning when the server listens beyond loopback without
// credentials.
//
// Deliberately a loud log line rather than a refusal: a user who has put this
// behind an authenticating reverse proxy is doing the right thing and should
// not be blocked. But nobody should end up exposed without having been told.
func WarnIfExposed(addr string, auth Auth) {
	if auth.Enabled() {
		return
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)

	loopbackOnly := host != "" && host != "0.0.0.0" && host != "[::]" && host != "::"
	if loopbackOnly {
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && !ip.IsLoopback() {
			loopbackOnly = false
		}
	}
	if loopbackOnly {
		return
	}

	log.Print("WARNING: listening on all interfaces with no authentication.")
	log.Print("         Anyone who can reach this port can see your Plex file paths")
	log.Print("         and database details, and can trigger a deep check.")
	log.Print("         Set PLEDEBE_USER and PLEDEBE_PASSWORD, or put pledebe behind")
	log.Print("         an authenticating reverse proxy. Never port-forward it.")
}
