// Package debugui is a barebones, unauthenticated developer UI for the
// backend: connect providers, import listens (optionally only the newest few),
// see counts, run the matching logic and inspect raw objects.
//
// Nothing else imports this package except server.New, which mounts it only
// when YOUNITHM_DEBUG_UI is set. Because it bypasses the admin login it
// refuses every request that does not come straight from this machine (see
// guard). See docs/development.md.
package debugui

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"younithm/internal/enrich"
	"younithm/internal/identity"
	"younithm/internal/imports"
	"younithm/internal/providers"
	"younithm/internal/providers/musicbrainz"
	"younithm/internal/secrets"
)

//go:embed index.html
var indexHTML []byte

// Deps is everything the debug UI drives. All of it already exists in
// server.New; the UI adds no business logic of its own.
type Deps struct {
	Read, Write *sql.DB
	Secrets     *secrets.Store
	Registry    *providers.Registry
	MusicBrainz *musicbrainz.Dynamic
	// MusicBrainzEnvContact is MUSICBRAINZ_CONTACT, the fallback contact.
	MusicBrainzEnvContact string
	// ITunesEnabled is YOUNITHM_ENABLE_ITUNES: the keyless iTunes fallback is
	// off unless it is set.
	ITunesEnabled bool
	Imports       *imports.Manager
	Backfill      *identity.Backfill
	Enrich        *enrich.Manager
	Review        *enrich.Review
	// Background bounds the work the UI starts (it outlives each request).
	Background context.Context
}

// Mount registers the UI at /debug/ (page) and /debug/api/ (JSON).
func Mount(r chi.Router, d Deps) {
	m := newMatching(d)
	pass := func(next http.Handler) http.Handler { return next } // the guard is the only gate
	r.With(guard).Get("/debug", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/debug/", http.StatusFound)
	})
	r.Route("/debug", func(r chi.Router) {
		r.Use(guard)
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(indexHTML)
		})
		r.Route("/api", func(r chi.Router) {
			r.Get("/connections", d.connections)
			r.Put("/connections/{name}", d.setConnection)
			r.Delete("/connections/{name}", d.deleteConnection)
			r.Post("/connections/{provider}/test", d.testConnection)

			r.Get("/stats", d.stats)
			r.Get("/list/{kind}", d.list)
			r.Get("/inspect/{kind}/{id}", d.inspect)

			r.Get("/matching", m.status)
			r.Post("/matching/run", m.run)

			// The existing admin endpoints, mounted without their login:
			//   /debug/api/v1/admin/imports...            upload, jobs, confirm
			//   /debug/api/v1/admin/identity/backfill...  dry run, cancel
			//   /debug/api/v1/admin/review, /enrichment   candidates, queue
			r.Route("/v1", func(r chi.Router) {
				d.Imports.MountWith(r, pass, pass)
				d.Backfill.Mount(r, pass, pass)
				d.Review.MountAt(r, "/admin", pass)
			})
		})
	})
	log.Printf("debug: UI enabled at /debug/ (NO LOGIN, loopback only). Unset YOUNITHM_DEBUG_UI to turn it off")
}

// guard is the only protection the debug UI has, so it is strict:
//   - the TCP peer must be a loopback address;
//   - no proxy headers: a reverse proxy on this machine would make an outside
//     request look loopback, so any forwarded request is refused;
//   - the Host header must be a loopback name (stops DNS rebinding);
//   - state-changing requests need the X-Debug-UI header (a cross-site page
//     cannot send it without a CORS preflight, which is never granted) and an
//     Origin, when present, that matches Host.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func(why string) {
			http.Error(w, "debug UI: "+why, http.StatusForbidden)
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			deny("only reachable from this machine (loopback)")
			return
		}
		for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded"} {
			if r.Header.Get(h) != "" {
				deny("proxied requests are refused")
				return
			}
		}
		if !loopbackName(r.Host) {
			deny("Host must be localhost, 127.0.0.1 or [::1]")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Debug-UI") != "1" {
				deny("missing X-Debug-UI header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					deny("cross-origin request")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackName(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func serverErr(w http.ResponseWriter, what string, err error) {
	log.Printf("debug: %s: %v", what, err)
	fail(w, http.StatusInternalServerError, what+": "+err.Error())
}
