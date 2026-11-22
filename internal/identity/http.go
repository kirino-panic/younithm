package identity

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Mount registers the admin backfill endpoints under /admin/identity:
//
//	GET  /admin/identity/backfill           {preview, run}: latest dry run and latest real run
//	POST /admin/identity/backfill/preview   start a dry run on a private copy (202)
//	POST /admin/identity/backfill           start the real run, body {"resume":bool} (202)
//	POST /admin/identity/backfill/cancel    stop the running job; resume continues it
//	GET  /admin/identity/integrity          rule checks and review counts (see Integrity)
func (b *Backfill) Mount(r chi.Router, session func(http.Handler) http.Handler) {
	r.Route("/admin/identity/backfill", func(r chi.Router) {
		r.Use(session)
		r.Get("/", b.status)
		r.Post("/preview", b.start(true))
		r.Post("/", b.start(false))
		r.Post("/cancel", b.cancelHTTP)
	})
	r.With(session).Get("/admin/identity/integrity", func(w http.ResponseWriter, r *http.Request) {
		in, err := CheckIntegrity(r.Context(), b.db)
		if err != nil {
			jsonErr(w, 500, "internal", "integrity check failed")
			return
		}
		jsonOut(w, 200, in)
	})
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, id, msg string) {
	jsonOut(w, code, map[string]string{"error": id, "message": msg})
}

func (b *Backfill) status(w http.ResponseWriter, r *http.Request) {
	prev, err := b.Latest(r.Context(), true)
	if err != nil {
		jsonErr(w, 500, "internal", "reading backfill status failed")
		return
	}
	run, err := b.Latest(r.Context(), false)
	if err != nil {
		jsonErr(w, 500, "internal", "reading backfill status failed")
		return
	}
	jsonOut(w, 200, map[string]any{"preview": prev, "run": run})
}

func (b *Backfill) start(dry bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Resume bool `json:"resume"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
				jsonErr(w, 400, "bad_request", "invalid JSON body")
				return
			}
		}
		// The job outlives the request, so it runs on the manager's context.
		id, err := b.Start(b.bg, dry, body.Resume)
		if errors.Is(err, ErrBackfillRunning) {
			jsonErr(w, 409, "already_running", err.Error())
			return
		}
		if err != nil {
			jsonErr(w, 500, "internal", "starting backfill failed")
			return
		}
		jsonOut(w, 202, map[string]any{"id": id, "dry_run": dry})
	}
}

func (b *Backfill) cancelHTTP(w http.ResponseWriter, r *http.Request) {
	if !b.Cancel() {
		jsonErr(w, 409, "not_running", "no backfill is running")
		return
	}
	jsonOut(w, 202, map[string]bool{"cancelling": true})
}
