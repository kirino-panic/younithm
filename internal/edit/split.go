package edit

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Split replaces artist `id`, whose name is really several artists ("Danny
// Brown & Underscores"), with the artists given, in order. Every track and
// album credited to it is credited to them instead, in its place in the
// credit list; the artist is deleted. Its name and its other spellings become
// split rules, so later listens credited that way get the same artists. Join
// phrases come from the input; the last artist takes the one the split artist
// had. One log entry, undoable.
func (e *Editor) Split(ctx context.Context, actor string, id int64, in []CreditIn) (*Result, error) {
	if len(in) < 2 {
		return nil, invalid("bad_value", "split into at least two artists")
	}
	if len(in) > 20 {
		return nil, invalid("bad_value", "too many artists")
	}
	for _, c := range in {
		if c.ArtistID == id {
			return nil, invalid("bad_value", "an artist cannot be split into itself")
		}
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, err := e.entityExists(ctx, tx, KindArtist, id)
		if err != nil {
			return nil, err
		}
		// A new artist named like the one being split would just be it again.
		for _, c := range in {
			if c.ArtistID == 0 && Norm(c.Name) == Norm(str(row["name"])) {
				return nil, invalid("bad_value", "an artist cannot be split into itself")
			}
		}
		pl := newPlanner(ctx, tx, e.now())
		ids, cs, err := pl.credits(in)
		if err != nil {
			return nil, err
		}
		joins := make([]any, len(ids))
		for i := range ids[:len(ids)-1] {
			if j := in[i].JoinPhrase; j != "" {
				joins[i] = j
			} else {
				joins[i] = ", "
			}
		}

		for _, t := range []struct{ table, col string }{{"recording_artist", "recording_id"}, {"release_artist", "release_id"}} {
			own, err := queryRows(ctx, tx, `SELECT `+cols(t.table)+` FROM `+t.table+` WHERE artist_id=?`, id)
			if err != nil {
				return nil, err
			}
			for _, x := range own {
				more, err := splitCredits(ctx, tx, t.table, t.col, x, ids, joins)
				if err != nil {
					return nil, err
				}
				cs = append(cs, more...)
			}
		}

		// The artist's names become split rules; its aliases go with it.
		keys := []string{ArtistKey(str(row["name"]))}
		if d := Norm(str(row["display_name"])); d != "" {
			keys = append(keys, d)
		}
		aliases, err := queryRows(ctx, tx, `SELECT `+cols("alias")+` FROM alias WHERE kind='artist' AND entity_id=?`, id)
		if err != nil {
			return nil, err
		}
		for _, a := range aliases {
			keys = append(keys, str(a["raw_string"]))
			cs = append(cs, Change{Table: "alias", Key: Row{"id": a["id"]}, Before: a})
		}
		others, err := otherNameChanges(ctx, tx, KindArtist, id, 0)
		if err != nil {
			return nil, err
		}
		cs = append(cs, others...)
		seen := map[string]bool{}
		for _, k := range keys {
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			old, err := queryRows(ctx, tx, `SELECT `+cols("artist_split")+` FROM artist_split WHERE raw_string=?`, k)
			if err != nil {
				return nil, err
			}
			for _, o := range old { // a newer split of the same string wins
				cs = append(cs, Change{Table: "artist_split", Key: pickKey(tables["artist_split"], o), Before: o})
			}
			for i, aid := range ids {
				n := Row{"raw_string": k, "position": int64(i), "artist_id": aid, "join_phrase": joins[i]}
				cs = append(cs, Change{Table: "artist_split", Key: pickKey(tables["artist_split"], n), After: n})
			}
		}
		// An older rule naming this artist as one of its parts would lose a
		// position when the artist goes; it is dropped whole instead.
		parts, err := queryRows(ctx, tx, `SELECT DISTINCT raw_string FROM artist_split WHERE artist_id=?`, id)
		if err != nil {
			return nil, err
		}
		for _, p := range parts {
			k := str(p["raw_string"])
			if seen[k] { // already replaced above
				continue
			}
			rows, err := queryRows(ctx, tx, `SELECT `+cols("artist_split")+` FROM artist_split WHERE raw_string=?`, k)
			if err != nil {
				return nil, err
			}
			for _, o := range rows {
				cs = append(cs, Change{Table: "artist_split", Key: pickKey(tables["artist_split"], o), Before: o})
			}
		}

		links, err := queryRows(ctx, tx, `SELECT `+cols("external_link")+` FROM external_link WHERE entity_type='artist' AND entity_id=?`, id)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			cs = append(cs, Change{Table: "external_link", Key: Row{"id": l["id"]}, Before: l})
		}
		rej, err := queryRows(ctx, tx, `SELECT `+cols("link_rejection")+` FROM link_rejection WHERE entity_type='artist' AND entity_id=?`, id)
		if err != nil {
			return nil, err
		}
		for _, r := range rej {
			cs = append(cs, Change{Table: "link_rejection", Key: pickKey(tables["link_rejection"], r), Before: r})
		}
		cs = append(cs, Change{Table: "artist", Key: Row{"id": id}, Before: row})

		names := make([]string, len(in))
		for i, c := range in {
			names[i] = strings.TrimSpace(c.Name)
			if c.ArtistID != 0 {
				names[i] = fmt.Sprintf("#%d", c.ArtistID)
			}
		}
		// Logged as a merge: undoing it is as big a change.
		return &draft{scope: "merge", entityType: KindArtist, entityID: id,
			summary: fmt.Sprintf("Split artist %d (%s) into %s", id, str(row["name"]), strings.Join(names, ", ")), changes: cs}, nil
	})
}

// splitCredits replaces credit row x (of the split artist) on one track or
// album with the given artists at its place, renumbering the rest. Artists
// already credited there keep their own row.
func splitCredits(ctx context.Context, tx Querier, table, col string, x Row, ids []int64, joins []any) ([]Change, error) {
	q := `SELECT ` + cols(table) + ` FROM ` + table + ` WHERE ` + col + `=?`
	args := []any{x[col]}
	if table == "recording_artist" {
		q += ` AND role=?`
		args = append(args, x["role"])
	}
	all, err := queryRows(ctx, tx, q, args...)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		pi, pj := all[i]["position"].(int64), all[j]["position"].(int64)
		if pi != pj {
			return pi < pj
		}
		return all[i]["artist_id"].(int64) < all[j]["artist_id"].(int64)
	})
	have := map[int64]bool{}
	for _, r := range all {
		have[r["artist_id"].(int64)] = true
	}
	var cs []Change
	pos := int64(0)
	for _, r := range all {
		if r["artist_id"] != x["artist_id"] {
			if r["position"] != pos {
				cs = append(cs, Change{Table: table, Key: pickKey(tables[table], r), Before: Row{"position": r["position"]}, After: Row{"position": pos}})
			}
			pos++
			continue
		}
		cs = append(cs, Change{Table: table, Key: pickKey(tables[table], r), Before: r})
		last := -1
		for i, aid := range ids {
			if !have[aid] {
				last = i
			}
		}
		for i, aid := range ids {
			if have[aid] {
				continue
			}
			n := Row{col: x[col], "artist_id": aid, "position": pos, "join_phrase": joins[i], "credited_name": nil}
			if i == last {
				n["join_phrase"] = x["join_phrase"]
			}
			if table == "recording_artist" {
				n["role"] = x["role"]
			}
			cs = append(cs, Change{Table: table, Key: pickKey(tables[table], n), After: n})
			pos++
		}
	}
	return cs, nil
}
