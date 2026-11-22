package edit

import (
	"context"
	"database/sql"
	"fmt"
)

func (e *Editor) needRecordingRelease(ctx context.Context, tx Querier, rec, rel int64) error {
	if _, err := e.entityExists(ctx, tx, KindRecording, rec); err != nil {
		return err
	}
	_, err := e.entityExists(ctx, tx, KindRelease, rel)
	return err
}

// AddToRelease puts a recording on an album (recording_release). The first
// album a recording gets becomes its primary release.
func (e *Editor) AddToRelease(ctx context.Context, actor string, rec, rel int64, disc int64, position *int64) (*Result, error) {
	if disc <= 0 {
		disc = 1
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		if err := e.needRecordingRelease(ctx, tx, rec, rel); err != nil {
			return nil, err
		}
		cs, err := newPlanner(ctx, tx, e.now()).link(rec, rel, disc, position)
		if err != nil {
			return nil, err
		}
		if len(cs) == 0 {
			return nil, &Error{Status: 409, Code: "already_on_release", Msg: "recording is already on that release"}
		}
		return &draft{scope: "album", entityType: KindRecording, entityID: rec,
			summary: fmt.Sprintf("Add recording %d to release %d", rec, rel), changes: cs}, nil
	})
}

// RemoveFromRelease takes a recording off an album. Scrobbles that recorded
// the play against that album are detached from it (release_id cleared); if it
// was the primary release, the earliest remaining album becomes primary.
func (e *Editor) RemoveFromRelease(ctx context.Context, actor string, rec, rel int64) (*Result, error) {
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, ok, err := readRow(ctx, tx, "recording_release", Row{"recording_id": rec, "release_id": rel}, tables["recording_release"].cols)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, notFound("recording %d is not on release %d", rec, rel)
		}
		cs := []Change{}
		scr, err := queryRows(ctx, tx, `SELECT id FROM scrobble WHERE recording_id=? AND release_id=?`, rec, rel)
		if err != nil {
			return nil, err
		}
		for _, s := range scr {
			cs = append(cs, Change{Table: "scrobble", Key: Row{"id": s["id"]}, Before: Row{"release_id": rel}, After: Row{"release_id": nil}})
		}
		cs = append(cs, Change{Table: "recording_release", Key: pickKey(tables["recording_release"], row), Before: row})
		if row["is_primary"] == int64(1) {
			next, err := queryRows(ctx, tx, `SELECT rr.release_id AS id FROM recording_release rr JOIN release r ON r.id=rr.release_id
				WHERE rr.recording_id=? AND rr.release_id<>? ORDER BY r.release_date IS NULL, r.release_date, r.id LIMIT 1`, rec, rel)
			if err != nil {
				return nil, err
			}
			if len(next) > 0 {
				cs = append(cs, Change{Table: "recording_release", Key: Row{"recording_id": rec, "release_id": next[0]["id"]},
					Before: Row{"is_primary": int64(0)}, After: Row{"is_primary": int64(1)}})
			}
		}
		return &draft{scope: "album", entityType: KindRecording, entityID: rec,
			summary: fmt.Sprintf("Remove recording %d from release %d", rec, rel), changes: cs}, nil
	})
}

// SetPrimaryRelease makes rel the recording's primary release (default
// artwork and display album).
func (e *Editor) SetPrimaryRelease(ctx context.Context, actor string, rec, rel int64) (*Result, error) {
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		rows, err := queryRows(ctx, tx, `SELECT release_id, is_primary FROM recording_release WHERE recording_id=?`, rec)
		if err != nil {
			return nil, err
		}
		found := false
		var cs []Change
		for _, r := range rows {
			isTarget := r["release_id"] == rel
			found = found || isTarget
			want := int64(0)
			if isTarget {
				want = 1
			}
			if r["is_primary"] != want {
				cs = append(cs, Change{Table: "recording_release", Key: Row{"recording_id": rec, "release_id": r["release_id"]},
					Before: Row{"is_primary": r["is_primary"]}, After: Row{"is_primary": want}})
			}
		}
		if !found {
			return nil, invalid("not_on_release", "recording %d is not on release %d", rec, rel)
		}
		return &draft{scope: "album", entityType: KindRecording, entityID: rec,
			summary: fmt.Sprintf("Set release %d as primary for recording %d", rel, rec), changes: cs}, nil
	})
}

// SetNoAlbum marks a track as on no album, or clears the mark. Marking it takes
// it off every album and clears the album of its scrobbles; while marked,
// nothing puts it on an album again (identity.resolveRelease and the catalogue
// gap-fill check the mark). Clearing it only lifts the mark: later listens and
// lookups may put it on an album again.
func (e *Editor) SetNoAlbum(ctx context.Context, actor string, rec int64, on bool) (*Result, error) {
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, err := e.entityExists(ctx, tx, KindRecording, rec)
		if err != nil {
			return nil, err
		}
		want := int64(0)
		if on {
			want = 1
		}
		if row["no_album"] == want {
			return nil, ErrNoChange
		}
		cs := []Change{{Table: "recording", Key: Row{"id": rec}, Before: Row{"no_album": row["no_album"]}, After: Row{"no_album": want}}}
		summary := fmt.Sprintf("Allow albums for recording %d", rec)
		if on {
			summary = fmt.Sprintf("Mark recording %d as on no album", rec)
			scr, err := queryRows(ctx, tx, `SELECT id, release_id FROM scrobble WHERE recording_id=? AND release_id IS NOT NULL`, rec)
			if err != nil {
				return nil, err
			}
			for _, s := range scr {
				cs = append(cs, Change{Table: "scrobble", Key: Row{"id": s["id"]}, Before: Row{"release_id": s["release_id"]}, After: Row{"release_id": nil}})
			}
			rels, err := queryRows(ctx, tx, `SELECT `+cols("recording_release")+` FROM recording_release WHERE recording_id=?`, rec)
			if err != nil {
				return nil, err
			}
			for _, r := range rels {
				cs = append(cs, Change{Table: "recording_release", Key: pickKey(tables["recording_release"], r), Before: r})
			}
		}
		return &draft{scope: "album", entityType: KindRecording, entityID: rec, summary: summary, changes: cs}, nil
	})
}
