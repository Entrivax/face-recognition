// Package web embeds the static web UI (HTML/CSS/JS) into the binary and
// serves it at the root. There is no build step: edit the files under static/
// and rebuild the Go binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// ServeUI serves the single-page UI at "/" and its assets.
func ServeUI(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}
