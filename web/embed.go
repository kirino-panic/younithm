// Package web serves the built frontend (web/dist, produced by `npm run build`)
// from inside the binary. Unknown paths fall back to index.html so client-side
// routes work on reload; API-shaped paths get a JSON 404 instead.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

var apiPrefixes = []string{"/api/", "/1/", "/apis/", "/debug/", "/healthz"}

// Handler serves static assets and the single-page app. base is the path the
// app is served under ("" at the root, else "/younithm"); index.html gets a
// <base href> for it, which the relative asset URLs and the app's own URL
// building both resolve against.
func Handler(base string) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	index, _ := fs.ReadFile(sub, "index.html")
	if index != nil {
		tag := []byte(`<head>` + "\n" + `    <base href="` + base + `/" />`)
		index = bytes.Replace(index, []byte("<head>"), tag, 1)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range apiPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not_found"}` + "\n"))
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" {
			if st, err := fs.Stat(sub, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		if index == nil {
			http.Error(w, "frontend not built: run `make web`", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
