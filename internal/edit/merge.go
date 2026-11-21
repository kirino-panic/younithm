package edit

import (
	"context"
	"database/sql"
	"fmt"
)

// Merge folds entity `from` into `into` (same kind): everything that pointed
// at `from` now points at `into`, `from` is deleted, and alias rows make the
// old names resolve to `into` for future ingest and imports. The whole merge
// is one log entry and can be undone as long as nothing touched the affected
// rows since.
func (e *Editor) Merge(ctx context.Context, actor, kind string, from, into int64) (*Result, error) {
	return e.MergeWith(ctx, actor, kind, from, into, MergeOptions{})
}

// MergeOptions changes how a merge is remembered.
type MergeOptions struct {
	// Once merges what is there now without redirecting the future: `from`'s
	// names, aliases and catalogue links are dropped instead of moving to
	// `into`, so a later listen named or linked like `from` makes a new entry.
	Once bool
}

// MergeWith is Merge with options.
func (e *Editor) MergeWith(ctx context.Context, actor, kind string, from, into int64, opt MergeOptions) (*Result, error) {
	if _, ok := kindTable[kind]; !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	if from == into {
		return nil, invalid("same_entity", "cannot merge an entity into itself")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		fromRow, err := e.entityExists(ctx, tx, kind, from)
		if err != nil {
			return nil, err
		}
		if _, err := e.entityExists(ctx, tx, kind, into); err != nil {
			return nil, err
		}
		var cs []Change
		if opt.Once {
			if cs, err = forgetEntity(ctx, tx, kind, from); err != nil {
				return nil, err
			}
		} else if cs, err = redirectEntity(ctx, tx, kind, fromRow, from, into); err != nil {
			return nil, err
		}

		switch kind {
		case KindArtist:
			cs, err = mergeArtist(ctx, tx, cs, from, into, opt.Once)
		case KindRecording:
			cs, err = mergeRecording(ctx, tx, cs, from, into)
		case KindRelease:
			cs, err = mergeRelease(ctx, tx, cs, from, into)
		}
		if err != nil {
			return nil, err
		}
		cs = append(cs, Change{Table: kindTable[kind], Key: Row{"id": from}, Before: fromRow})
		summary := fmt.Sprintf("Merge %s %d into %d", kind, from, into)
		if opt.Once {
			summary += " (this once)"
		}
		return &draft{scope: "merge", entityType: kind, entityID: into, summary: summary, changes: cs}, nil
	})
}

// forgetEntity drops `from`'s aliases and catalogue links, for a merge that
// is not remembered.
func forgetEntity(ctx context.Context, tx Querier, kind string, from int64) ([]Change, error) {
	var cs []Change
	aliases, err := queryRows(ctx, tx, `SELECT `+cols("alias")+` FROM alias WHERE kind=? AND entity_id=?`, kind, from)
	if err != nil {
		return nil, err
	}
	for _, a := range aliases {
		cs = append(cs, Change{Table: "alias", Key: Row{"id": a["id"]}, Before: a})
	}
	links, err := queryRows(ctx, tx, `SELECT `+cols("external_link")+` FROM external_link WHERE entity_type=? AND entity_id=?`, kind, from)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		cs = append(cs, Change{Table: "external_link", Key: Row{"id": l["id"]}, Before: l})
	}
	return cs, nil
}

// redirectEntity moves `from`'s aliases and links to `into` and adds `from`'s
// names as aliases of `into`.
func redirectEntity(ctx context.Context, tx Querier, kind string, fromRow Row, from, into int64) ([]Change, error) {
	var cs []Change
	// Aliases first (keys are derived from `from` as it is now).
	keys, err := aliasKeys(ctx, tx, kind, from, fromRow)
	if err != nil {
		return nil, err
	}
	// Existing aliases that pointed at `from` follow it.
	old, err := queryRows(ctx, tx, `SELECT id, entity_id FROM alias WHERE kind=? AND entity_id=?`, kind, from)
	if err != nil {
		return nil, err
	}
	for _, a := range old {
		cs = append(cs, Change{Table: "alias", Key: Row{"id": a["id"]}, Before: Row{"entity_id": from}, After: Row{"entity_id": into}})
	}
	ac, err := aliasChanges(ctx, tx, kind, keys, into, true)
	if err != nil {
		return nil, err
	}
	// aliasChanges may also repoint a row already moved above; drop those.
	moved := map[any]bool{}
	for _, a := range old {
		moved[canon(a["id"])] = true
	}
	for _, c := range ac {
		if c.Before != nil && moved[canon(c.Key["id"])] {
			continue
		}
		cs = append(cs, c)
	}

	// External links follow the entity; duplicates are dropped.
	links, err := queryRows(ctx, tx, `SELECT `+cols("external_link")+` FROM external_link WHERE entity_type=? AND entity_id=?`, kind, from)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		dup, err := queryRows(ctx, tx, `SELECT id FROM external_link WHERE entity_type=? AND entity_id=? AND provider=? AND provider_id=?`,
			kind, into, l["provider"], l["provider_id"])
		if err != nil {
			return nil, err
		}
		if len(dup) > 0 {
			cs = append(cs, Change{Table: "external_link", Key: Row{"id": l["id"]}, Before: l})
		} else {
			cs = append(cs, Change{Table: "external_link", Key: Row{"id": l["id"]}, Before: Row{"entity_id": from}, After: Row{"entity_id": into}})
		}
	}
	return cs, nil
}

func cols(table string) string {
	s := ""
	for i, c := range tables[table].cols {
		if i > 0 {
			s += ", "
		}
		s += c
	}
	return s
}

// moveCredit repoints a (recording|release)_artist row from artist `from` to
// `into`, or drops it when `into` already has an equivalent row.
func mergeArtist(ctx context.Context, tx Querier, cs []Change, from, into int64, once bool) ([]Change, error) {
	// Split rules naming `from` name `into` instead; a rule that would then
	// name `into` twice, or any rule at all for a merge done once, is dropped.
	rules, err := queryRows(ctx, tx, `SELECT DISTINCT raw_string FROM artist_split WHERE artist_id=?`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		rows, err := queryRows(ctx, tx, `SELECT `+cols("artist_split")+` FROM artist_split WHERE raw_string=?`, r["raw_string"])
		if err != nil {
			return nil, err
		}
		drop := once
		for _, o := range rows {
			if o["artist_id"] == into {
				drop = true
			}
		}
		for _, o := range rows {
			switch {
			case drop:
				cs = append(cs, Change{Table: "artist_split", Key: pickKey(tables["artist_split"], o), Before: o})
			case o["artist_id"] == from:
				cs = append(cs, Change{Table: "artist_split", Key: pickKey(tables["artist_split"], o), Before: Row{"artist_id": from}, After: Row{"artist_id": into}})
			}
		}
	}

	ra, err := queryRows(ctx, tx, `SELECT `+cols("recording_artist")+` FROM recording_artist WHERE artist_id=?`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range ra {
		cs = append(cs, Change{Table: "recording_artist", Key: pickKey(tables["recording_artist"], r), Before: r})
		_, has, err := readRow(ctx, tx, "recording_artist", Row{"recording_id": r["recording_id"], "artist_id": into, "role": r["role"]}, []string{"role"})
		if err != nil {
			return nil, err
		}
		if !has {
			n := copyRow(r)
			n["artist_id"] = into
			cs = append(cs, Change{Table: "recording_artist", Key: pickKey(tables["recording_artist"], n), After: n})
		}
	}
	rl, err := queryRows(ctx, tx, `SELECT `+cols("release_artist")+` FROM release_artist WHERE artist_id=?`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range rl {
		cs = append(cs, Change{Table: "release_artist", Key: pickKey(tables["release_artist"], r), Before: r})
		_, has, err := readRow(ctx, tx, "release_artist", Row{"release_id": r["release_id"], "artist_id": into}, []string{"release_id"})
		if err != nil {
			return nil, err
		}
		if !has {
			n := copyRow(r)
			n["artist_id"] = into
			cs = append(cs, Change{Table: "release_artist", Key: pickKey(tables["release_artist"], n), After: n})
		}
	}
	return cs, nil
}

func copyRow(r Row) Row {
	n := Row{}
	for k, v := range r {
		n[k] = v
	}
	return n
}

func repointScrobbles(ctx context.Context, tx Querier, cs []Change, col string, from, into int64) ([]Change, error) {
	rows, err := queryRows(ctx, tx, `SELECT id FROM scrobble WHERE `+col+`=?`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		cs = append(cs, Change{Table: "scrobble", Key: Row{"id": r["id"]}, Before: Row{col: from}, After: Row{col: into}})
	}
	return cs, nil
}

func mergeRecording(ctx context.Context, tx Querier, cs []Change, from, into int64) ([]Change, error) {
	cs, err := repointScrobbles(ctx, tx, cs, "recording_id", from, into)
	if err != nil {
		return nil, err
	}
	// Album membership: `into` keeps its own primary; gain the other albums.
	hasPrimary, err := queryRows(ctx, tx, `SELECT 1 AS x FROM recording_release WHERE recording_id=? AND is_primary=1`, into)
	if err != nil {
		return nil, err
	}
	primaryTaken := len(hasPrimary) > 0
	rr, err := queryRows(ctx, tx, `SELECT `+cols("recording_release")+` FROM recording_release WHERE recording_id=? ORDER BY is_primary DESC, release_id`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range rr {
		cs = append(cs, Change{Table: "recording_release", Key: pickKey(tables["recording_release"], r), Before: r})
		ex, has, err := readRow(ctx, tx, "recording_release", Row{"recording_id": into, "release_id": r["release_id"]}, []string{"is_primary"})
		if err != nil {
			return nil, err
		}
		if has && !primaryTaken && r["is_primary"] == int64(1) {
			primaryTaken = true
			cs = append(cs, Change{Table: "recording_release", Key: Row{"recording_id": into, "release_id": r["release_id"]},
				Before: Row{"is_primary": ex["is_primary"]}, After: Row{"is_primary": int64(1)}})
		}
		if !has {
			n := copyRow(r)
			n["recording_id"] = into
			if primaryTaken {
				n["is_primary"] = int64(0)
			} else if n["is_primary"] == int64(1) {
				primaryTaken = true
			}
			cs = append(cs, Change{Table: "recording_release", Key: pickKey(tables["recording_release"], n), After: n})
		}
	}
	// Credits: keep `into`'s; adopt `from`'s only if `into` has none.
	intoCred, err := queryRows(ctx, tx, `SELECT 1 AS x FROM recording_artist WHERE recording_id=?`, into)
	if err != nil {
		return nil, err
	}
	ra, err := queryRows(ctx, tx, `SELECT `+cols("recording_artist")+` FROM recording_artist WHERE recording_id=?`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range ra {
		cs = append(cs, Change{Table: "recording_artist", Key: pickKey(tables["recording_artist"], r), Before: r})
		if len(intoCred) == 0 {
			n := copyRow(r)
			n["recording_id"] = into
			cs = append(cs, Change{Table: "recording_artist", Key: pickKey(tables["recording_artist"], n), After: n})
		}
	}
	return cs, nil
}

func mergeRelease(ctx context.Context, tx Querier, cs []Change, from, into int64) ([]Change, error) {
	cs, err := repointScrobbles(ctx, tx, cs, "release_id", from, into)
	if err != nil {
		return nil, err
	}
	rr, err := queryRows(ctx, tx, `SELECT `+cols("recording_release")+` FROM recording_release WHERE release_id=?`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range rr {
		cs = append(cs, Change{Table: "recording_release", Key: pickKey(tables["recording_release"], r), Before: r})
		ex, has, err := readRow(ctx, tx, "recording_release", Row{"recording_id": r["recording_id"], "release_id": into}, []string{"is_primary"})
		if err != nil {
			return nil, err
		}
		if has && r["is_primary"] == int64(1) && ex["is_primary"] != int64(1) {
			// The primary album is being merged away: its replacement inherits the role.
			cs = append(cs, Change{Table: "recording_release", Key: Row{"recording_id": r["recording_id"], "release_id": into},
				Before: Row{"is_primary": ex["is_primary"]}, After: Row{"is_primary": int64(1)}})
		}
		if !has {
			n := copyRow(r)
			n["release_id"] = into
			if n["is_primary"] == int64(1) {
				other, err := queryRows(ctx, tx, `SELECT 1 AS x FROM recording_release WHERE recording_id=? AND is_primary=1 AND release_id<>?`, r["recording_id"], from)
				if err != nil {
					return nil, err
				}
				if len(other) > 0 {
					n["is_primary"] = int64(0)
				}
			}
			cs = append(cs, Change{Table: "recording_release", Key: pickKey(tables["recording_release"], n), After: n})
		}
	}
	intoArt, err := queryRows(ctx, tx, `SELECT 1 AS x FROM release_artist WHERE release_id=?`, into)
	if err != nil {
		return nil, err
	}
	ra, err := queryRows(ctx, tx, `SELECT `+cols("release_artist")+` FROM release_artist WHERE release_id=?`, from)
	if err != nil {
		return nil, err
	}
	for _, r := range ra {
		cs = append(cs, Change{Table: "release_artist", Key: pickKey(tables["release_artist"], r), Before: r})
		if len(intoArt) == 0 {
			n := copyRow(r)
			n["release_id"] = into
			cs = append(cs, Change{Table: "release_artist", Key: pickKey(tables["release_artist"], n), After: n})
		}
	}
	return cs, nil
}
