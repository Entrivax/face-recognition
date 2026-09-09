// Package web embeds the built web UI (Preact + TypeScript, built by Vite
// from web/) into the binary and serves it at the root. The bundle lives in
// dist/ (gitignored except for a placeholder); rebuild it with `make ui`
// (npm --prefix web run build) before building the Go binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// ServeUI serves the single-page UI at "/" and its assets. Vite-hashed
// assets under /assets/ are immutable; index.html is revalidated so a new
// bundle is picked up after an upgrade.
func ServeUI(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}

	// Fresh clone without a built UI: dist/ only holds the placeholder.
	if _, err := sub.Open("index.html"); err != nil {
		http.Error(w, "web UI not built — run: make ui", http.StatusServiceUnavailable)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}
