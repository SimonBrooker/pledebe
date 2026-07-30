package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// Icons and the web manifest are embedded, so they ship inside the binary. No
// extra volume to mount, and nothing fetched from the internet — which matters
// for a tool that may be running on a network with no route out.
//
//go:embed static
var staticFS embed.FS

// staticCacheControl is long and immutable because these files only change when
// the binary does, and a new binary serves them from a new container. The page
// itself is deliberately no-store; this overrides that for assets alone.
const staticCacheControl = "public, max-age=604800, immutable"

// staticHandler serves the embedded assets.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Only reachable if the embed directive and the directory disagree,
		// which is a build-time mistake rather than a runtime condition.
		panic("embedded static assets missing: " + err.Error())
	}

	files := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", http.HandlerFunc(
		func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Cache-Control", staticCacheControl)
			files.ServeHTTP(w, req)
		}))
}

// serveIcon answers /favicon.ico with the small PNG.
//
// Browsers request /favicon.ico unprompted, before parsing any <link>. Serving
// the 32px PNG there avoids a 404 on every first visit; modern browsers accept
// a PNG regardless of the .ico extension.
func serveIcon(w http.ResponseWriter, req *http.Request) {
	raw, err := staticFS.ReadFile("static/icon-32.png")
	if err != nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", staticCacheControl)
	_, _ = w.Write(raw)
}

// serveManifest answers the PWA manifest with its own media type, which
// http.FileServer will not infer from the .webmanifest extension.
func serveManifest(w http.ResponseWriter, req *http.Request) {
	raw, err := staticFS.ReadFile("static/manifest.webmanifest")
	if err != nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", staticCacheControl)
	_, _ = w.Write(raw)
}
