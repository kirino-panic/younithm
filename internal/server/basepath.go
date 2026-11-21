package server

import (
	"net/http"
	"strings"
)

// withBasePath lets the app live under a path such as /younithm. Requests
// under the prefix have it removed before routing, so every handler keeps
// its root-relative routes. Requests without it pass through unchanged:
// some proxies strip the prefix themselves, and the container healthcheck
// calls /healthz directly.
func withBasePath(base string, h http.Handler) http.Handler {
	if base == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == base {
			u := base + "/"
			if r.URL.RawQuery != "" {
				u += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, u, http.StatusPermanentRedirect)
			return
		}
		if !strings.HasPrefix(p, base+"/") {
			h.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p[len(base):]
		if rp := r.URL.RawPath; rp != "" && strings.HasPrefix(rp, base+"/") {
			r2.URL.RawPath = rp[len(base):]
		} else {
			r2.URL.RawPath = ""
		}
		r2.RequestURI = r2.URL.RequestURI()
		h.ServeHTTP(w, r2)
	})
}
