package ingest

import (
	"database/sql"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	lbDefaultCount = 25
	lbMaxCount     = 1000
)

// userListens serves GET /1/user/{user}/listens, which clients such as
// Multi-Scrobbler read before submitting to check for duplicates.
//
// A valid scrobble token reads the history even when the scrobble list is
// private. Without one the request goes through the list gate, and when the
// gate refuses, the reply is an empty listen list instead of an error, so
// clients that only look for duplicates keep scrobbling (the store still
// dedupes on insert).
func (h *Handler) userListens(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	count := lbDefaultCount
	if v := q.Get("count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			lbError(w, 400, "Invalid count.")
			return
		}
		count = min(n, lbMaxCount)
	}
	parseTS := func(k string) (int64, bool, bool) {
		v := q.Get(k)
		if v == "" {
			return 0, false, true
		}
		n, err := strconv.ParseInt(v, 10, 64)
		return n, true, err == nil && n >= 0
	}
	minTS, hasMin, ok1 := parseTS("min_ts")
	maxTS, hasMax, ok2 := parseTS("max_ts")
	if !ok1 || !ok2 {
		lbError(w, 400, "min_ts and max_ts must be Unix timestamps.")
		return
	}
	if hasMin && hasMax && minTS >= maxTS {
		lbError(w, 400, "min_ts must be less than max_ts.")
		return
	}
	user := chi.URLParam(r, "user")

	serve := func() {
		var where []string
		args := []any{}
		where = append(where, "user_id = 1")
		if hasMin {
			where = append(where, "played_at > ?")
			args = append(args, time.Unix(minTS, 0).UTC().Format(TimeFormat))
		}
		if hasMax {
			where = append(where, "played_at < ?")
			args = append(args, time.Unix(maxTS, 0).UTC().Format(TimeFormat))
		}
		args = append(args, count)
		rows, err := h.read.QueryContext(r.Context(),
			`SELECT id, played_at, created_at, raw_artist, raw_title, raw_album, source, duration_played_ms
			 FROM scrobble WHERE `+strings.Join(where, " AND ")+` ORDER BY played_at DESC, id DESC LIMIT ?`, args...)
		if err != nil {
			log.Printf("ingest: lb listens: %v", err)
			lbError(w, 500, "Internal error.")
			return
		}
		defer rows.Close()
		listens := []map[string]any{}
		for rows.Next() {
			var (
				id                          int64
				playedAt, createdAt, artist string
				title                       string
				album, source               sql.NullString
				dur                         sql.NullInt64
			)
			if err := rows.Scan(&id, &playedAt, &createdAt, &artist, &title, &album, &source, &dur); err != nil {
				log.Printf("ingest: lb listens scan: %v", err)
				lbError(w, 500, "Internal error.")
				return
			}
			info := map[string]any{}
			if source.Valid && source.String != "" {
				info["submission_client"] = source.String
			}
			if dur.Valid && dur.Int64 > 0 {
				info["duration_ms"] = dur.Int64
			}
			md := map[string]any{"artist_name": artist, "track_name": title, "additional_info": info}
			if album.Valid && album.String != "" {
				md["release_name"] = album.String
			}
			listens = append(listens, map[string]any{
				"listened_at":    unixOf(playedAt),
				"inserted_at":    unixOf(createdAt),
				"recording_msid": strconv.FormatInt(id, 10),
				"user_name":      user,
				"track_metadata": md,
			})
		}
		if err := rows.Err(); err != nil {
			log.Printf("ingest: lb listens rows: %v", err)
			lbError(w, 500, "Internal error.")
			return
		}
		var newest, oldest sql.NullString
		_ = h.read.QueryRowContext(r.Context(),
			`SELECT MAX(played_at), MIN(played_at) FROM scrobble WHERE user_id = 1`).Scan(&newest, &oldest)
		writeLBListens(w, user, listens, unixOf(newest.String), unixOf(oldest.String))
	}

	if h.canReadHistory(r) {
		serve()
		return
	}
	reached := false
	h.listGate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
		serve()
	})).ServeHTTP(discardWriter{h: http.Header{}}, r)
	if !reached {
		writeLBListens(w, user, []map[string]any{}, 0, 0)
	}
}

// canReadHistory reports whether the request carries a real scrobble token.
// When the admin accepts scrobbles without one, any token passes the
// Authenticator, so a token then proves nothing and the list gate decides.
func (h *Handler) canReadHistory(r *http.Request) bool {
	tok, ok := bearer(r)
	if !ok || tok == "" {
		return false
	}
	if _, _, err := h.auth.AuthenticateToken(r.Context(), tok); err != nil {
		return false
	}
	_, _, err := h.auth.AuthenticateToken(r.Context(), "")
	return err != nil
}

func writeLBListens(w http.ResponseWriter, user string, listens []map[string]any, newest, oldest int64) {
	writeJSON(w, 200, map[string]any{"payload": map[string]any{
		"count": len(listens), "listens": listens, "user_id": user,
		"latest_listen_ts": newest, "oldest_listen_ts": oldest,
	}})
}

func unixOf(s string) int64 {
	if s == "" {
		return 0
	}
	for _, f := range []string{TimeFormat, time.RFC3339Nano} {
		if t, err := time.Parse(f, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// discardWriter swallows what a refusing gate writes.
type discardWriter struct{ h http.Header }

func (d discardWriter) Header() http.Header         { return d.h }
func (d discardWriter) Write(b []byte) (int, error) { return io.Discard.Write(b) }
func (d discardWriter) WriteHeader(int)             {}
