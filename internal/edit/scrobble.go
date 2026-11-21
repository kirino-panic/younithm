package edit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"younithm/internal/names"
)

// NullableInt distinguishes "absent" from "null" from a value in JSON bodies.
type NullableInt struct {
	Set   bool
	Valid bool // false with Set = explicit null
	V     int64
}

func (n *NullableInt) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Valid = false
		return nil
	}
	n.Valid = true
	return json.Unmarshal(b, &n.V)
}

// ScrobbleEdit is a partial update of one scrobble. Raw values are not
// editable; RecordingID/ReleaseID change what the scrobble points at.
type ScrobbleEdit struct {
	PlayedAt     *time.Time  `json:"played_at"`
	ShiftSeconds *int64      `json:"shift_seconds"` // bulk only; relative to current time
	RecordingID  NullableInt `json:"recording_id"`
	ReleaseID    NullableInt `json:"release_id"`
	// LinkRelease adds the recording to the release (recording_release) if it
	// is not already on it, instead of refusing.
	LinkRelease bool `json:"link_release"`
	// NewRecording creates a track (and any artists that do not exist yet) and
	// points the scrobble at it; NewRelease creates an album for the recording
	// and does the same. A track with that title and exactly those artists is
	// reused instead of created. Both are single-scrobble only and part of the same
	// undoable edit.
	NewRecording *NewRecording `json:"new_recording"`
	NewRelease   string        `json:"new_release"`
}

// NewRecording is a track to create: a title and its credited artists.
type NewRecording struct {
	Title   string     `json:"title"`
	Artists []CreditIn `json:"artists"`
}

var scrobbleCols = tables["scrobble"].cols

type planner struct {
	tx     Querier
	ctx    context.Context
	now    time.Time
	linked map[[2]int64]bool // recording_release inserts already planned this edit
	next   map[string]int64  // next free id per table for rows planned this edit
	// Rows planned but not yet in the database.
	newRec, newRel int64
	reuseRec       int64 // an existing track that matched new_recording
	newRecArtists  []Row // release_artist template (artist_id, position, join_phrase) of newRec
}

// nextID hands out ids for rows this edit will insert.
func (p *planner) nextID(table string) (int64, error) {
	if p.next == nil {
		p.next = map[string]int64{}
	}
	if _, ok := p.next[table]; !ok {
		var n int64
		if err := p.tx.QueryRowContext(p.ctx, `SELECT COALESCE(MAX(id),0)+1 FROM `+table).Scan(&n); err != nil {
			return 0, err
		}
		p.next[table] = n
	}
	id := p.next[table]
	p.next[table]++
	return id, nil
}

// credits resolves artists given by id or name: an existing artist with
// exactly that name is reused, otherwise one is planned for creation.
func (p *planner) credits(in []CreditIn) (ids []int64, cs []Change, err error) {
	ids = make([]int64, len(in))
	seen := map[int64]bool{}
	for i, c := range in {
		name := strings.TrimSpace(c.Name)
		switch {
		case c.ArtistID > 0:
			ok, err := p.exists("artist", c.ArtistID)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				return nil, nil, notFound("artist %d not found", c.ArtistID)
			}
			ids[i] = c.ArtistID
		case name != "":
			rows, err := queryRows(p.ctx, p.tx, `SELECT id FROM artist WHERE name=? ORDER BY id LIMIT 2`, name)
			if err != nil {
				return nil, nil, err
			}
			if len(rows) == 1 {
				ids[i] = rows[0]["id"].(int64)
				break
			}
			// Created earlier in this same edit under that name?
			for _, ch := range cs {
				if ch.Table == "artist" && ch.After["name"] == name {
					ids[i] = ch.After["id"].(int64)
				}
			}
			if ids[i] != 0 {
				break
			}
			id, err := p.nextID("artist")
			if err != nil {
				return nil, nil, err
			}
			ids[i] = id
			cs = append(cs, Change{Table: "artist", Key: Row{"id": id}, After: Row{"id": id, "name": name}})
		default:
			return nil, nil, invalid("bad_value", "artist %d has no id or name", i+1)
		}
		if seen[ids[i]] {
			return nil, nil, invalid("bad_value", "an artist is listed twice")
		}
		seen[ids[i]] = true
	}
	return ids, cs, nil
}

// createRecording plans a new track with its credits.
func (p *planner) createRecording(nr NewRecording) ([]Change, error) {
	title := strings.TrimSpace(nr.Title)
	if title == "" {
		return nil, invalid("bad_value", "track title is empty")
	}
	if len(nr.Artists) == 0 {
		return nil, invalid("bad_value", "a new track needs at least one artist")
	}
	if len(nr.Artists) > 50 {
		return nil, invalid("bad_value", "too many artists")
	}
	ids, cs, err := p.credits(nr.Artists)
	if err != nil {
		return nil, err
	}
	// An existing track with this title and exactly these artists is reused.
	if len(cs) == 0 {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprint(id)
		}
		rows, err := queryRows(p.ctx, p.tx, `SELECT r.id FROM recording r WHERE lower(r.title)=lower(?)
			AND (SELECT group_concat(artist_id) FROM (SELECT artist_id FROM recording_artist WHERE recording_id=r.id ORDER BY position))=?
			ORDER BY r.id LIMIT 1`, title, strings.Join(parts, ","))
		if err != nil {
			return nil, err
		}
		if len(rows) == 1 {
			p.reuseRec = rows[0]["id"].(int64)
			return nil, nil
		}
	}
	rec, err := p.nextID("recording")
	if err != nil {
		return nil, err
	}
	cs = append(cs, Change{Table: "recording", Key: Row{"id": rec}, After: Row{"id": rec, "title": title}})
	p.newRec, p.newRecArtists = rec, nil
	for i, aid := range ids {
		var jp any
		if i < len(nr.Artists)-1 && nr.Artists[i].JoinPhrase != "" {
			jp = nr.Artists[i].JoinPhrase
		}
		n := Row{"recording_id": rec, "artist_id": aid, "role": "primary", "position": int64(i), "join_phrase": jp}
		cs = append(cs, Change{Table: "recording_artist", Key: pickKey(tables["recording_artist"], n), After: n})
		p.newRecArtists = append(p.newRecArtists, Row{"artist_id": aid, "position": int64(i), "join_phrase": jp})
	}
	return cs, nil
}

// createRelease plans a new album titled title whose artists are rec's.
func (p *planner) createRelease(rec int64, title string) ([]Change, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, invalid("bad_value", "album title is empty")
	}
	artists := p.newRecArtists
	if rec != p.newRec {
		var err error
		artists, err = queryRows(p.ctx, p.tx, `SELECT artist_id, position, join_phrase, credited_name FROM recording_artist WHERE recording_id=? ORDER BY position`, rec)
		if err != nil {
			return nil, err
		}
	}
	rel, err := p.nextID("release")
	if err != nil {
		return nil, err
	}
	p.newRel = rel
	cs := []Change{{Table: "release", Key: Row{"id": rel}, After: Row{"id": rel, "title": title}}}
	for _, a := range artists {
		n := Row{"release_id": rel, "artist_id": a["artist_id"], "position": a["position"], "join_phrase": a["join_phrase"], "credited_name": a["credited_name"]}
		cs = append(cs, Change{Table: "release_artist", Key: pickKey(tables["release_artist"], n), After: n})
	}
	return cs, nil
}

func (p *planner) exists(table string, id int64) (bool, error) {
	_, ok, err := readRow(p.ctx, p.tx, table, Row{"id": id}, []string{"id"})
	return ok, err
}

func asInt(v any) (int64, bool) {
	i, ok := v.(int64)
	return i, ok
}

// onRelease reports whether recording rec is linked to release rel.
func (p *planner) onRelease(rec, rel int64) (bool, error) {
	_, ok, err := readRow(p.ctx, p.tx, "recording_release", Row{"recording_id": rec, "release_id": rel}, []string{"recording_id"})
	return ok || p.linked[[2]int64{rec, rel}], err
}

// scrobble plans the changes for editing one scrobble row.
func (p *planner) scrobble(row Row, ed ScrobbleEdit) ([]Change, error) {
	id := row["id"]
	before, after := Row{}, Row{}
	set := func(col string, v any) {
		if canon(row[col]) != canon(v) {
			before[col], after[col] = row[col], v
		}
	}
	var extra []Change

	curPlayed, _ := row["played_at"].(string)
	newPlayed := curPlayed
	if ed.PlayedAt != nil {
		newPlayed = ed.PlayedAt.UTC().Format(TimeFormat)
	} else if ed.ShiftSeconds != nil {
		t, err := time.Parse(TimeFormat, curPlayed)
		if err != nil {
			return nil, fmt.Errorf("scrobble %v: bad played_at %q", id, curPlayed)
		}
		newPlayed = t.Add(time.Duration(*ed.ShiftSeconds) * time.Second).UTC().Format(TimeFormat)
	}
	if newPlayed != curPlayed {
		set("played_at", newPlayed)
		if row["orig_played_at"] == nil {
			set("orig_played_at", curPlayed)
		}
	}

	if ed.NewRecording != nil {
		if ed.RecordingID.Set {
			return nil, invalid("bad_request", "recording_id and new_recording are exclusive")
		}
		c, err := p.createRecording(*ed.NewRecording)
		if err != nil {
			return nil, err
		}
		extra = append(extra, c...)
		id := p.newRec
		if p.reuseRec != 0 {
			id = p.reuseRec
		}
		ed.RecordingID = NullableInt{Set: true, Valid: true, V: id}
	}

	rec, _ := asInt(row["recording_id"])
	rel, _ := asInt(row["release_id"])
	recValid, relValid := row["recording_id"] != nil, row["release_id"] != nil
	if ed.RecordingID.Set {
		recValid, rec = ed.RecordingID.Valid, ed.RecordingID.V
		if recValid && (p.newRec == 0 || rec != p.newRec) {
			ok, err := p.exists("recording", rec)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, invalid("unknown_recording", "recording %d does not exist", rec)
			}
		}
		set("recording_id", nullIf(recValid, rec))
	}
	if ed.NewRelease != "" {
		if ed.ReleaseID.Set {
			return nil, invalid("bad_request", "release_id and new_release are exclusive")
		}
		if !recValid {
			return nil, invalid("release_needs_recording", "a scrobble needs a recording before it can have an album")
		}
		c, err := p.createRelease(rec, ed.NewRelease)
		if err != nil {
			return nil, err
		}
		extra = append(extra, c...)
		ed.ReleaseID = NullableInt{Set: true, Valid: true, V: p.newRel}
		ed.LinkRelease = true
	}
	if ed.ReleaseID.Set {
		relValid, rel = ed.ReleaseID.Valid, ed.ReleaseID.V
		if relValid && (p.newRel == 0 || rel != p.newRel) {
			ok, err := p.exists("release", rel)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, invalid("unknown_release", "release %d does not exist", rel)
			}
		}
	}
	// The album played from must be an album the recording is on.
	if relValid {
		if !recValid {
			if ed.ReleaseID.Set {
				return nil, invalid("release_needs_recording", "a scrobble needs a recording before it can have an album")
			}
			relValid = false // recording cleared: drop the stale album
		} else if ok, err := p.onRelease(rec, rel); err != nil {
			return nil, err
		} else if !ok {
			switch {
			case ed.ReleaseID.Set && ed.LinkRelease:
				c, err := p.link(rec, rel, 1, nil)
				if err != nil {
					return nil, err
				}
				extra = append(extra, c...)
			case ed.ReleaseID.Set:
				return nil, invalid("not_on_release", "recording %d is not on release %d (set link_release to add it)", rec, rel)
			default:
				relValid = false // recording changed to one that is not on the old album
			}
		}
	}
	if ed.ReleaseID.Set || ed.RecordingID.Set {
		set("release_id", nullIf(relValid, rel))
	}

	if len(after) == 0 && len(extra) == 0 {
		return nil, nil
	}
	cs := append([]Change{}, extra...)
	if len(after) > 0 {
		before["edited_at"], after["edited_at"] = row["edited_at"], p.now.Format("2006-01-02T15:04:05.000Z")
		cs = append(cs, Change{Table: "scrobble", Key: Row{"id": id}, Before: before, After: after})
	}
	return cs, nil
}

func nullIf(valid bool, v int64) any {
	if !valid {
		return nil
	}
	return v
}

// link plans adding recording rec to release rel if it is not there. The first
// release a recording gets becomes its primary.
func (p *planner) link(rec, rel int64, disc int64, pos *int64) ([]Change, error) {
	if ok, err := p.onRelease(rec, rel); err != nil || ok {
		return nil, err
	}
	rows, err := queryRows(p.ctx, p.tx, `SELECT 1 AS x FROM recording_release WHERE recording_id=? AND is_primary=1`, rec)
	if err != nil {
		return nil, err
	}
	prim := int64(1)
	if len(rows) > 0 || p.hasPlannedPrimary(rec) {
		prim = 0
	}
	if prim == 1 {
		p.markPrimary(rec)
	}
	p.linked[[2]int64{rec, rel}] = true
	var position any
	if pos != nil {
		position = *pos
	}
	return []Change{{Table: "recording_release", Key: Row{"recording_id": rec, "release_id": rel},
		After: Row{"recording_id": rec, "release_id": rel, "disc": disc, "position": position, "is_primary": prim}}}, nil
}

func (p *planner) hasPlannedPrimary(rec int64) bool { return p.linked[[2]int64{rec, -1}] }
func (p *planner) markPrimary(rec int64)            { p.linked[[2]int64{rec, -1}] = true }

func newPlanner(ctx context.Context, tx Querier, now time.Time) *planner {
	return &planner{tx: tx, ctx: ctx, now: now, linked: map[[2]int64]bool{}}
}

func loadScrobble(ctx context.Context, tx Querier, id int64) (Row, error) {
	row, ok, err := readRow(ctx, tx, "scrobble", Row{"id": id}, scrobbleCols)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("scrobble %d not found", id)
	}
	return row, nil
}

// EditScrobble changes one scrobble's time and/or what it points at.
func (e *Editor) EditScrobble(ctx context.Context, actor string, id int64, ed ScrobbleEdit) (*Result, error) {
	if ed.ShiftSeconds != nil {
		return nil, invalid("bad_request", "shift_seconds is only available in bulk edits")
	}
	if ed.NewRecording != nil {
		ed.NewRecording.Title = strings.TrimSpace(ed.NewRecording.Title)
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, err := loadScrobble(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		cs, err := newPlanner(ctx, tx, e.now()).scrobble(row, ed)
		if err != nil {
			return nil, err
		}
		return &draft{scope: "scrobble", entityType: "scrobble", entityID: id,
			summary: fmt.Sprintf("Edit scrobble %d", id), changes: cs}, nil
	})
}

// MoveScrobbleToRelease points a scrobble at another album its recording is
// on (or, with link, adds the recording to it first).
func (e *Editor) MoveScrobbleToRelease(ctx context.Context, actor string, id, releaseID int64, link bool) (*Result, error) {
	res, err := e.EditScrobble(ctx, actor, id, ScrobbleEdit{ReleaseID: NullableInt{Set: true, Valid: true, V: releaseID}, LinkRelease: link})
	return res, err
}

// DeleteScrobble removes a scrobble; the full row (raw values included) is
// kept in the log so undo restores it with the same id.
func (e *Editor) DeleteScrobble(ctx context.Context, actor string, id int64) (*Result, error) {
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, err := loadScrobble(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		return &draft{scope: "scrobble", entityType: "scrobble", entityID: id,
			summary: fmt.Sprintf("Delete scrobble %d", id), changes: []Change{{Table: "scrobble", Key: Row{"id": id}, Before: row}}}, nil
	})
}

// ScrobbleView is the admin-only raw-vs-effective view of a scrobble.
type ScrobbleView struct {
	ID           int64   `json:"id"`
	PlayedAt     string  `json:"played_at"`
	OrigPlayedAt *string `json:"orig_played_at"`
	EditedAt     *string `json:"edited_at"`
	Raw          struct {
		Artist  string  `json:"artist"`
		Title   string  `json:"title"`
		Album   *string `json:"album"`
		Payload *string `json:"payload"`
	} `json:"raw"`
	Effective struct {
		Artist      string  `json:"artist"`
		Title       string  `json:"title"`
		Album       *string `json:"album"`
		RecordingID *int64  `json:"recording_id"`
		ReleaseID   *int64  `json:"release_id"`
	} `json:"effective"`
}

// View loads the raw and effective values of a scrobble.
func View(ctx context.Context, q Querier, id int64) (*ScrobbleView, error) {
	var v ScrobbleView
	err := q.QueryRowContext(ctx, `SELECT s.id, s.played_at, s.orig_played_at, s.edited_at, s.raw_artist, s.raw_title, s.raw_album, s.raw_payload,
		COALESCE(`+names.LineSQL(names.Recording, "s.recording_id")+`, s.raw_artist),
		COALESCE(rec.title, s.raw_title), COALESCE(rel.title, s.raw_album), s.recording_id, s.release_id
		FROM scrobble s LEFT JOIN recording rec ON rec.id=s.recording_id LEFT JOIN release rel ON rel.id=s.release_id WHERE s.id=?`, id).
		Scan(&v.ID, &v.PlayedAt, &v.OrigPlayedAt, &v.EditedAt, &v.Raw.Artist, &v.Raw.Title, &v.Raw.Album, &v.Raw.Payload,
			&v.Effective.Artist, &v.Effective.Title, &v.Effective.Album, &v.Effective.RecordingID, &v.Effective.ReleaseID)
	if err == sql.ErrNoRows {
		return nil, notFound("scrobble %d not found", id)
	}
	return &v, err
}
