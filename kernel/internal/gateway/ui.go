package gateway

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The control plane UI (ui/ at the repo root, React + Vite) is built into
// ui/dist and embedded here, so the whole app ships inside the single
// `aura` executable. Rebuild with: cd ui && npm run build.

//go:embed all:ui/dist
var uiFS embed.FS

func (g *Gateway) ui(w http.ResponseWriter, r *http.Request) {
	dist, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		http.Error(w, "ui not embedded", http.StatusInternalServerError)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if _, err := fs.Stat(dist, path); err != nil {
		// SPA fallback: unknown non-API paths render the app shell.
		path = "index.html"
	}
	http.ServeFileFS(w, r, dist, path)
}
