package debugui

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

const artistNames = `COALESCE((SELECT group_concat(name, ' / ') FROM (
	SELECT a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
	WHERE ra.recording_id=%s ORDER BY ra.position, a.id)), '')`

// list pages through objects of one kind, for finding something to inspect.
//
//	GET /debug/api/list/{artist|recording|release|scrobble}?q=&sort=&limit=&offset=
//
// q matches names/titles (substring), an exact provider id (MBID, Spotify id,
// ISRC ...) or a numeric id.
func (d Deps) list(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := 50, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = n
	}
	sortPlays := r.URL.Query().Get("sort") != "id"
	id, _ := strconv.ParseInt(q, 10, 64)
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"

	var sel, where, order string
	var args []any
	byLink := `EXISTS (SELECT 1 FROM external_link l WHERE l.entity_type=? AND l.entity_id=x.id AND l.provider_id=?)`
	byAlias := `EXISTS (SELECT 1 FROM alias al WHERE al.kind=? AND al.entity_id=x.id AND al.raw_string LIKE ? ESCAPE '\')`
	switch kind {
	case "artist":
		sel = `SELECT x.id AS artist_id, x.name, x.verified,
			(SELECT COUNT(*) FROM external_link l WHERE l.entity_type='artist' AND l.entity_id=x.id) AS links,
			(SELECT COUNT(*) FROM recording_artist ra WHERE ra.artist_id=x.id) AS recordings,
			(SELECT COUNT(*) FROM scrobble s JOIN recording_artist ra ON ra.recording_id=s.recording_id WHERE ra.artist_id=x.id) AS plays
			FROM artist x`
		if q != "" {
			where = ` WHERE x.name LIKE ? ESCAPE '\' OR x.id=? OR ` + byLink + ` OR ` + byAlias
			args = []any{like, id, "artist", q, "artist", like}
		}
	case "recording":
		sel = `SELECT x.id AS recording_id, x.title, ` + strings.Replace(artistNames, "%s", "x.id", 1) + ` AS artists, x.duration_ms,
			(SELECT COUNT(*) FROM external_link l WHERE l.entity_type='recording' AND l.entity_id=x.id) AS links,
			(SELECT COUNT(*) FROM recording_release rr WHERE rr.recording_id=x.id) AS releases,
			(SELECT COUNT(*) FROM scrobble s WHERE s.recording_id=x.id) AS plays
			FROM recording x`
		if q != "" {
			where = ` WHERE x.title LIKE ? ESCAPE '\' OR x.id=? OR ` + byLink + ` OR ` + byAlias +
				` OR EXISTS (SELECT 1 FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=x.id AND a.name LIKE ? ESCAPE '\')`
			args = []any{like, id, "recording", q, "recording", like, like}
		}
	case "release":
		sel = `SELECT x.id AS release_id, x.title, x.release_date, x.release_type,
			COALESCE((SELECT group_concat(name, ' / ') FROM (SELECT a.name FROM release_artist ra JOIN artist a ON a.id=ra.artist_id
				WHERE ra.release_id=x.id ORDER BY ra.position, a.id)), '') AS album_artists,
			(SELECT COUNT(*) FROM external_link l WHERE l.entity_type='release' AND l.entity_id=x.id) AS links,
			(SELECT COUNT(*) FROM recording_release rr WHERE rr.release_id=x.id) AS recordings,
			(SELECT COUNT(*) FROM scrobble s WHERE s.release_id=x.id) AS plays
			FROM release x`
		if q != "" {
			where = ` WHERE x.title LIKE ? ESCAPE '\' OR x.id=? OR ` + byLink + ` OR ` + byAlias
			args = []any{like, id, "release", q, "release", like}
		}
	case "scrobble":
		sel = `SELECT x.id AS scrobble_id, x.played_at, x.raw_artist, x.raw_title, x.raw_album, x.source,
			x.recording_id, x.release_id, x.import_job_id FROM scrobble x`
		if q != "" {
			where = ` WHERE x.raw_artist LIKE ? ESCAPE '\' OR x.raw_title LIKE ? ESCAPE '\' OR x.raw_album LIKE ? ESCAPE '\' OR x.id=?`
			args = []any{like, like, like, id}
		}
		order = ` ORDER BY x.played_at DESC, x.id DESC`
	default:
		fail(w, http.StatusNotFound, "kind must be artist, recording, release or scrobble")
		return
	}
	if order == "" {
		if sortPlays {
			order = ` ORDER BY plays DESC, x.id`
		} else {
			order = ` ORDER BY x.id`
		}
	}
	rows, err := query(r.Context(), d.Read, sel+where+order+` LIMIT ? OFFSET ?`, append(args, limit+1, offset)...)
	if err != nil {
		serverErr(w, "list", err)
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "items": rows, "limit": limit, "offset": offset, "more": more})
}

// section runs one association query and keeps the first error.
type collector struct {
	ctx context.Context
	d   Deps
	out row // sections in the order they were added
	err error
}

func (c *collector) put(name string, v any) {
	c.out.cols = append(c.out.cols, name)
	c.out.vals = append(c.out.vals, v)
}

func (c *collector) add(name, q string, args ...any) {
	if c.err != nil {
		return
	}
	rs, err := query(c.ctx, c.d.Read, q, args...)
	if err != nil {
		c.err = err
		return
	}
	c.put(name, rs)
}

func (c *collector) addCount(name, q string, args ...any) {
	if c.err != nil {
		return
	}
	n, err := count(c.ctx, c.d.Read, q, args...)
	if err != nil {
		c.err = err
		return
	}
	c.put(name, n)
}

// entityParts are the association sections every entity kind shares. kind is
// artist, recording or release (also the value stored in entity_type/kind).
func (c *collector) entityParts(kind string, id int64) {
	c.add("external_links",
		`SELECT * FROM external_link WHERE entity_type=? AND entity_id=? ORDER BY provider, status, id`, kind, id)
	c.add("aliases", `SELECT * FROM alias WHERE kind=? AND entity_id=? ORDER BY id`, kind, id)
	c.add("link_rejections", `SELECT * FROM link_rejection WHERE entity_type=? AND entity_id=?`, kind, id)
	c.add("merge_candidates",
		`SELECT * FROM merge_candidate WHERE kind=? AND (entity_id=? OR other_id=?) ORDER BY id`, kind, id, id)
	// What each provider answered for the ids this entity is linked to, as
	// cached; raw_json is the provider's own response.
	c.add("provider_snapshots",
		`SELECT ps.* FROM provider_snapshot ps
		   WHERE ps.kind=? AND EXISTS (SELECT 1 FROM external_link l WHERE l.entity_type=? AND l.entity_id=?
		     AND l.provider=ps.provider AND l.provider_id=ps.provider_id)
		   ORDER BY ps.provider, ps.provider_id`, kind, kind, id)
	c.add("edit_log", `SELECT * FROM edit_log WHERE entity_type=? AND entity_id=? ORDER BY id DESC LIMIT 50`, kind, id)
}

const recentScrobbles = `SELECT id AS scrobble_id, played_at, duration_played_ms, source, recording_id, release_id,
	raw_artist, raw_title, raw_album, import_job_id FROM scrobble WHERE %s ORDER BY played_at DESC, id DESC LIMIT 25`

// inspect returns one object with every association, as raw rows.
//
//	GET /debug/api/inspect/{artist|recording|release|scrobble}/{id}
func (d Deps) inspect(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	ctx := r.Context()
	table := map[string]string{"artist": "artist", "recording": "recording", "release": "release", "scrobble": "scrobble"}[kind]
	if table == "" {
		fail(w, http.StatusNotFound, "kind must be artist, recording, release or scrobble")
		return
	}
	self, err := one(ctx, d.Read, `SELECT * FROM `+table+` WHERE id=?`, id)
	if err != nil {
		serverErr(w, "inspect", err)
		return
	}
	if self == nil {
		// A merged-away entity leaves nothing behind but its edit-log entry.
		fail(w, http.StatusNotFound, kind+" "+strconv.FormatInt(id, 10)+" does not exist")
		return
	}
	c := &collector{ctx: ctx, d: d}
	c.put("kind", kind)
	c.put(kind, self)

	switch kind {
	case "artist":
		c.addCount("play_count", `SELECT COUNT(*) FROM scrobble s JOIN recording_artist ra ON ra.recording_id=s.recording_id WHERE ra.artist_id=?`, id)
		c.add("recordings", `SELECT ra.recording_id AS recording_id, rec.title, ra.role, ra.position, ra.join_phrase,
			(SELECT COUNT(*) FROM scrobble s WHERE s.recording_id=rec.id) AS plays
			FROM recording_artist ra JOIN recording rec ON rec.id=ra.recording_id
			WHERE ra.artist_id=? ORDER BY plays DESC, rec.id LIMIT 200`, id)
		c.add("releases_as_album_artist", `SELECT ra.release_id AS release_id, rel.title, ra.position, ra.join_phrase
			FROM release_artist ra JOIN release rel ON rel.id=ra.release_id WHERE ra.artist_id=? ORDER BY rel.id LIMIT 200`, id)
		c.entityParts(kind, id)
	case "recording":
		c.addCount("play_count", `SELECT COUNT(*) FROM scrobble WHERE recording_id=?`, id)
		c.add("artists", `SELECT ra.artist_id AS artist_id, a.name, ra.role, ra.position, ra.join_phrase, a.verified
			FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? ORDER BY ra.position, a.id`, id)
		c.add("releases", `SELECT rr.release_id AS release_id, rel.title, rel.release_date, rel.release_type,
			rr.disc, rr.position, rr.is_primary FROM recording_release rr JOIN release rel ON rel.id=rr.release_id
			WHERE rr.recording_id=? ORDER BY rr.is_primary DESC, rel.id`, id)
		c.add("enrichment_job", `SELECT * FROM enrichment_job WHERE recording_id=?`, id)
		c.add("recent_scrobbles", strings.Replace(recentScrobbles, "%s", "recording_id=?", 1), id)
		c.entityParts(kind, id)
	case "release":
		c.addCount("play_count", `SELECT COUNT(*) FROM scrobble WHERE release_id=?`, id)
		c.add("album_artists", `SELECT ra.artist_id AS artist_id, a.name, ra.position, ra.join_phrase, a.verified
			FROM release_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.release_id=? ORDER BY ra.position, a.id`, id)
		c.add("recordings", `SELECT rr.recording_id AS recording_id, rec.title, rr.disc, rr.position, rr.is_primary,
			(SELECT COUNT(*) FROM scrobble s WHERE s.recording_id=rec.id AND s.release_id=rr.release_id) AS plays_on_this_release
			FROM recording_release rr JOIN recording rec ON rec.id=rr.recording_id
			WHERE rr.release_id=? ORDER BY rr.disc, rr.position, rec.id LIMIT 500`, id)
		c.add("recent_scrobbles", strings.Replace(recentScrobbles, "%s", "release_id=?", 1), id)
		c.entityParts(kind, id)
	case "scrobble":
		var rec, rel int64
		if v, ok := self.get("recording_id").(int64); ok {
			rec = v
		}
		if v, ok := self.get("release_id").(int64); ok {
			rel = v
		}
		c.add("recording", `SELECT id AS recording_id, title, duration_ms, fuzzy_consent FROM recording WHERE id=?`, rec)
		c.add("recording_artists", `SELECT ra.artist_id AS artist_id, a.name, ra.role, ra.position, ra.join_phrase
			FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? ORDER BY ra.position, a.id`, rec)
		c.add("release", `SELECT id AS release_id, title, release_date, release_type FROM release WHERE id=?`, rel)
		c.add("release_artists", `SELECT ra.artist_id AS artist_id, a.name, ra.position FROM release_artist ra
			JOIN artist a ON a.id=ra.artist_id WHERE ra.release_id=? ORDER BY ra.position, a.id`, rel)
		c.add("import_job", `SELECT id AS import_job_id, status, format, filename, options_json, created_at FROM import_job
			WHERE id=(SELECT import_job_id FROM scrobble WHERE id=?)`, id)
		c.add("edit_log", `SELECT * FROM edit_log WHERE entity_type='scrobble' AND entity_id=? ORDER BY id DESC LIMIT 50`, id)
	}
	if c.err != nil {
		serverErr(w, "inspect", c.err)
		return
	}
	writeJSON(w, http.StatusOK, c.out)
}
