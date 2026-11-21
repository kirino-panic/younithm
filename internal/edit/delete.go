package edit

import (
	"context"
	"database/sql"
	"fmt"
)

// MaxEntityDeleteScrobbles caps how many scrobbles one entity delete may take
// with it, as a bulk edit is capped.
const MaxEntityDeleteScrobbles = 50000

// DeleteEntity deletes an artist, album or track and every scrobble of it:
//
//   - a track: its scrobbles, then the track;
//   - an album: the scrobbles played from it, the tracks that were only on it
//     and have no scrobbles left, then the album;
//   - an artist: the scrobbles of every track it is credited on, the tracks
//     and albums credited to it alone, its credit on shared ones, then the
//     artist. A scrobble left on a deleted album loses its album.
//
// Names, aliases, links and split rules of what is deleted go too, so a later
// listen named the same way makes a new entry. One log entry, undoable.
func (e *Editor) DeleteEntity(ctx context.Context, actor, kind string, id int64) (*Result, error) {
	if _, ok := kindTable[kind]; !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, err := e.entityExists(ctx, tx, kind, id)
		if err != nil {
			return nil, err
		}
		d := &deleter{ctx: ctx, tx: tx, done: map[string]bool{}}
		switch kind {
		case KindRecording:
			err = d.recording(id)
		case KindRelease:
			err = d.release(id)
		case KindArtist:
			err = d.artist(id)
		}
		if err != nil {
			return nil, err
		}
		if d.scrobbles > MaxEntityDeleteScrobbles {
			return nil, invalid("too_many", "this would delete %d scrobbles; at most %d at once", d.scrobbles, MaxEntityDeleteScrobbles)
		}
		name := str(row["name"])
		if kind != KindArtist {
			name = str(row["title"])
		}
		// Logged as bulk: undoing it is as big a change, so it needs step-up.
		return &draft{scope: "bulk", entityType: kind, entityID: id,
			summary: fmt.Sprintf("Delete %s %d (%s) and %d scrobbles", kind, id, name, d.scrobbles), changes: d.cs}, nil
	})
}

type deleter struct {
	ctx       context.Context
	tx        Querier
	cs        []Change
	done      map[string]bool // table + key of rows already deleted or changed
	scrobbles int
}

// drop deletes a row once, however many paths reach it.
func (d *deleter) drop(table string, r Row) {
	k := pickKey(tables[table], r)
	id := table + canon(k)
	if d.done[id] {
		return
	}
	d.done[id] = true
	if table == "scrobble" {
		d.scrobbles++
	}
	d.cs = append(d.cs, Change{Table: table, Key: k, Before: r})
}

func (d *deleter) dropWhere(table, where string, args ...any) error {
	rows, err := queryRows(d.ctx, d.tx, `SELECT `+cols(table)+` FROM `+table+` WHERE `+where, args...)
	if err != nil {
		return err
	}
	for _, r := range rows {
		d.drop(table, r)
	}
	return nil
}

// names drops what makes later listens find the entity: aliases, links and
// link rejections.
func (d *deleter) names(kind string, id int64) error {
	if err := d.dropWhere("alias", `kind=? AND entity_id=?`, kind, id); err != nil {
		return err
	}
	if err := d.dropWhere("external_link", `entity_type=? AND entity_id=?`, kind, id); err != nil {
		return err
	}
	return d.dropWhere("link_rejection", `entity_type=? AND entity_id=?`, kind, id)
}

func (d *deleter) recording(id int64) error {
	for _, w := range []struct{ table, where string }{
		{"scrobble", `recording_id=?`},
		{"recording_artist", `recording_id=?`},
		{"recording_release", `recording_id=?`},
	} {
		if err := d.dropWhere(w.table, w.where, id); err != nil {
			return err
		}
	}
	if err := d.names(KindRecording, id); err != nil {
		return err
	}
	return d.dropWhere("recording", `id=?`, id)
}

// release deletes an album with the scrobbles played from it.
func (d *deleter) release(id int64) error {
	if err := d.dropWhere("scrobble", `release_id=?`, id); err != nil {
		return err
	}
	// Tracks only on this album with nothing else to show for them go too.
	recs, err := queryRows(d.ctx, d.tx, `SELECT recording_id AS id FROM recording_release WHERE release_id=?`, id)
	if err != nil {
		return err
	}
	for _, r := range recs {
		rec := r["id"].(int64)
		other, err := queryRows(d.ctx, d.tx, `SELECT 1 AS x FROM recording_release WHERE recording_id=? AND release_id<>? LIMIT 1`, rec, id)
		if err != nil {
			return err
		}
		if len(other) > 0 {
			continue
		}
		left, err := d.scrobblesLeft(`recording_id=?`, rec)
		if err != nil {
			return err
		}
		if left == 0 {
			if err := d.recording(rec); err != nil {
				return err
			}
		}
	}
	return d.releaseRow(id)
}

// releaseRow deletes the album itself; scrobbles still on it lose their album.
func (d *deleter) releaseRow(id int64) error {
	rows, err := queryRows(d.ctx, d.tx, `SELECT id FROM scrobble WHERE release_id=?`, id)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if d.done["scrobble"+canon(Row{"id": r["id"]})] {
			continue
		}
		d.done["scrobble"+canon(Row{"id": r["id"]})] = true
		d.cs = append(d.cs, Change{Table: "scrobble", Key: Row{"id": r["id"]}, Before: Row{"release_id": id}, After: Row{"release_id": nil}})
	}
	for _, t := range []string{"release_artist", "recording_release"} {
		if err := d.dropWhere(t, `release_id=?`, id); err != nil {
			return err
		}
	}
	if err := d.names(KindRelease, id); err != nil {
		return err
	}
	return d.dropWhere("release", `id=?`, id)
}

func (d *deleter) artist(id int64) error {
	recs, err := queryRows(d.ctx, d.tx, `SELECT DISTINCT recording_id AS id FROM recording_artist WHERE artist_id=?`, id)
	if err != nil {
		return err
	}
	for _, r := range recs {
		rec := r["id"].(int64)
		if err := d.dropWhere("scrobble", `recording_id=?`, rec); err != nil {
			return err
		}
		if alone, err := d.only("recording_artist", "recording_id", rec, id); err != nil {
			return err
		} else if alone {
			if err := d.recording(rec); err != nil {
				return err
			}
		}
	}
	rels, err := queryRows(d.ctx, d.tx, `SELECT DISTINCT release_id AS id FROM release_artist WHERE artist_id=?`, id)
	if err != nil {
		return err
	}
	for _, r := range rels {
		rel := r["id"].(int64)
		if alone, err := d.only("release_artist", "release_id", rel, id); err != nil {
			return err
		} else if alone {
			if err := d.releaseRow(rel); err != nil {
				return err
			}
		}
	}
	for _, t := range []string{"recording_artist", "release_artist"} {
		if err := d.dropWhere(t, `artist_id=?`, id); err != nil {
			return err
		}
	}
	// A split rule naming this artist would lose a position: drop it whole.
	if err := d.dropWhere("artist_split", `raw_string IN (SELECT raw_string FROM artist_split WHERE artist_id=?)`, id); err != nil {
		return err
	}
	if err := d.names(KindArtist, id); err != nil {
		return err
	}
	return d.dropWhere("artist", `id=?`, id)
}

// only reports whether artist is the sole artist credited on the track or album.
func (d *deleter) only(table, col string, entity, artist int64) (bool, error) {
	rows, err := queryRows(d.ctx, d.tx, `SELECT 1 AS x FROM `+table+` WHERE `+col+`=? AND artist_id<>? LIMIT 1`, entity, artist)
	return len(rows) == 0, err
}

// scrobblesLeft counts the scrobbles matching where that this delete keeps.
func (d *deleter) scrobblesLeft(where string, args ...any) (int, error) {
	rows, err := queryRows(d.ctx, d.tx, `SELECT id FROM scrobble WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if !d.done["scrobble"+canon(Row{"id": r["id"]})] {
			n++
		}
	}
	return n, nil
}
