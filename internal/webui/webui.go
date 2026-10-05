package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed assets
var assets embed.FS

// Handler serves the Devdooth landing page and its demo assets.
//
// The same page and assets are published to GitHub Pages from docs/, so the
// page uses relative "assets/..." paths that resolve in both places. A test
// keeps the copies in sync.
func Handler() http.Handler {
	assetsFS, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("devdooth: embedded assets missing: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assetsFS))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	return mux
}
