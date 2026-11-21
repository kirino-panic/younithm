package edit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CreditIn is one artist of a credit a person sets: an existing artist by id,
// or a name (an artist with exactly that name is reused, else one is created).
type CreditIn struct {
	ArtistID   int64  `json:"artist_id,omitempty"`
	Name       string `json:"name,omitempty"`
	JoinPhrase string `json:"join_phrase,omitempty"`
}

// SetCredits replaces the artists of a recording or release, in order.
func (e *Editor) SetCredits(ctx context.Context, actor, kind string, id int64, in []CreditIn) (*Result, error) {
	table := map[string]string{KindRecording: "recording_artist", KindRelease: "release_artist"}[kind]
	col := map[string]string{KindRecording: "recording_id", KindRelease: "release_id"}[kind]
	if table == "" {
		return nil, invalid("bad_kind", "kind must be recording or release")
	}
	if len(in) == 0 {
		return nil, invalid("bad_value", "at least one artist is needed")
	}
	if len(in) > 50 {
		return nil, invalid("bad_value", "too many artists")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		if _, err := e.entityExists(ctx, tx, kind, id); err != nil {
			return nil, err
		}
		pl := newPlanner(ctx, tx, e.now())
		ids, cs, err := pl.credits(in)
		if err != nil {
			return nil, err
		}
		old, err := queryRows(ctx, tx, `SELECT `+cols(table)+` FROM `+table+` WHERE `+col+`=?`, id)
		if err != nil {
			return nil, err
		}
		// Rows kept (same key) are updated in place; the rest removed or added.
		want := map[string]Row{}
		var order []string
		for i, aid := range ids {
			var jp any
			if i < len(in)-1 && in[i].JoinPhrase != "" {
				jp = in[i].JoinPhrase
			}
			n := Row{col: id, "artist_id": aid, "position": int64(i), "join_phrase": jp}
			if kind == KindRecording {
				n["role"] = "primary"
			}
			k := canon(pickKey(tables[table], n))
			want[k] = n
			order = append(order, k)
		}
		var changed bool
		for _, r := range old {
			k := canon(pickKey(tables[table], r))
			n, keep := want[k]
			if !keep {
				cs = append(cs, Change{Table: table, Key: pickKey(tables[table], r), Before: r})
				changed = true
				continue
			}
			before, after := Row{}, Row{}
			for _, c := range []string{"position", "join_phrase"} {
				if canon(r[c]) != canon(n[c]) {
					before[c], after[c] = r[c], n[c]
				}
			}
			if len(after) > 0 {
				cs = append(cs, Change{Table: table, Key: pickKey(tables[table], r), Before: before, After: after})
				changed = true
			}
			delete(want, k)
		}
		for _, k := range order {
			if n, ok := want[k]; ok {
				cs = append(cs, Change{Table: table, Key: pickKey(tables[table], n), After: n})
				changed = true
			}
		}
		if !changed {
			return nil, ErrNoChange
		}
		return &draft{scope: "credits", entityType: kind, entityID: id,
			summary: fmt.Sprintf("Set artists of %s %d", kind, id), changes: cs}, nil
	})
}

// AddToNewRelease creates an album with title and puts the recording on it.
func (e *Editor) AddToNewRelease(ctx context.Context, actor string, rec int64, title string) (*Result, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, invalid("bad_value", "album title is empty")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		if _, err := e.entityExists(ctx, tx, KindRecording, rec); err != nil {
			return nil, err
		}
		var rel int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0)+1 FROM release`).Scan(&rel); err != nil {
			return nil, err
		}
		cs := []Change{{Table: "release", Key: Row{"id": rel}, After: Row{"id": rel, "title": title}}}
		// The recording's artists become the new album's artists.
		ra, err := queryRows(ctx, tx, `SELECT artist_id, position, join_phrase FROM recording_artist WHERE recording_id=? ORDER BY position`, rec)
		if err != nil {
			return nil, err
		}
		for _, a := range ra {
			n := Row{"release_id": rel, "artist_id": a["artist_id"], "position": a["position"], "join_phrase": a["join_phrase"]}
			cs = append(cs, Change{Table: "release_artist", Key: pickKey(tables["release_artist"], n), After: n})
		}
		lc, err := newPlanner(ctx, tx, e.now()).link(rec, rel, 1, nil)
		if err != nil {
			return nil, err
		}
		cs = append(cs, lc...)
		return &draft{scope: "album", entityType: KindRecording, entityID: rec,
			summary: fmt.Sprintf("Add recording %d to new release %q", rec, title), changes: cs}, nil
	})
}

// SearchHit is one autocomplete suggestion.
type SearchHit struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Sub  string `json:"sub,omitempty"`
}

// Search suggests artists or releases whose name (or a name they were
// submitted under) contains q, best matches first.
func Search(ctx context.Context, db Querier, kind, q string, limit int) ([]SearchHit, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []SearchHit{}, nil
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
	prefix := strings.ToLower(q) + "%"
	var query string
	switch kind {
	case KindArtist:
		query = `SELECT a.id, a.name,
  (SELECT COUNT(*) FROM recording_artist ra WHERE ra.artist_id=a.id) || ' tracks'
FROM artist a
WHERE lower(a.name) LIKE ?1 ESCAPE '\' OR EXISTS (SELECT 1 FROM alias al WHERE al.kind='artist' AND al.entity_id=a.id AND lower(al.raw_string) LIKE ?1 ESCAPE '\')
ORDER BY lower(a.name) = lower(?3) DESC, lower(a.name) LIKE ?2 DESC,
  (SELECT COUNT(*) FROM recording_artist ra WHERE ra.artist_id=a.id) DESC, a.name LIMIT ?4`
	case KindRelease:
		query = `SELECT r.id, r.title,
  trim(COALESCE((SELECT group_concat(name, ', ') FROM (SELECT a.name FROM release_artist ra JOIN artist a ON a.id=ra.artist_id
     WHERE ra.release_id=r.id ORDER BY ra.position)), '') || COALESCE(' · ' || substr(r.release_date,1,4), ''), ' ·')
FROM release r
WHERE lower(r.title) LIKE ?1 ESCAPE '\' OR EXISTS (SELECT 1 FROM alias al WHERE al.kind='release' AND al.entity_id=r.id AND lower(al.raw_string) LIKE ?1 ESCAPE '\')
ORDER BY lower(r.title) = lower(?3) DESC, lower(r.title) LIKE ?2 DESC, r.title LIMIT ?4`
	case KindRecording:
		query = `SELECT r.id, r.title,
  trim(COALESCE((SELECT group_concat(name, ', ') FROM (SELECT a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
     WHERE ra.recording_id=r.id ORDER BY ra.position)), '') || COALESCE(' · ' || (SELECT rel.title FROM recording_release rr JOIN release rel ON rel.id=rr.release_id
     WHERE rr.recording_id=r.id ORDER BY rr.is_primary DESC, rel.id LIMIT 1), ''), ' ·')
FROM recording r
WHERE lower(r.title) LIKE ?1 ESCAPE '\' OR EXISTS (SELECT 1 FROM alias al WHERE al.kind='recording' AND al.entity_id=r.id AND lower(replace(al.raw_string, char(31), ' ')) LIKE ?1 ESCAPE '\')
  OR EXISTS (SELECT 1 FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=r.id AND lower(a.name || ' ' || r.title) LIKE ?1 ESCAPE '\')
ORDER BY lower(r.title) = lower(?3) DESC, lower(r.title) LIKE ?2 DESC,
  (SELECT COUNT(*) FROM scrobble s WHERE s.recording_id=r.id) DESC, r.title LIMIT ?4`
	default:
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	rows, err := db.QueryContext(ctx, query, like, prefix, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		var sub sql.NullString
		if err := rows.Scan(&h.ID, &h.Name, &sub); err != nil {
			return nil, err
		}
		h.Sub = sub.String
		out = append(out, h)
	}
	return out, rows.Err()
}
