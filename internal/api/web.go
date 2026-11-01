package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The UI ships inside the binary: no build step, nothing to mount, and the image
// can't drift from the code that serves it.
//
//go:embed web
var webFiles embed.FS

// webFS is web/ as the root, so "index.html" and "app.js" resolve directly.
var webFS = mustSub(webFiles, "web")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err) // only possible if the embed directive and dir disagree
	}
	return sub
}

func index(w http.ResponseWriter, r *http.Request) {
	// Revalidate on every load so a new rollout's UI is picked up immediately.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, webFS, "index.html")
}

// static serves the JS and CSS. Directory paths 404 rather than getting a listing.
func static() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(webFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
