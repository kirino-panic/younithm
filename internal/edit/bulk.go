package edit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MaxBulk is the largest number of scrobbles one bulk edit may touch.
const MaxBulk = 50000

// Filter selects scrobbles for a bulk edit. At least one criterion is
// required, so an empty filter can never mean "everything".
type Filter struct {
	IDs         []int64 `json:"ids"`
	From        string  `json:"from"` // RFC 3339, inclusive
	To          string  `json:"to"`   // RFC 3339, exclusive
	Source      string  `json:"source"`
	RawArtist   string  `json:"raw_artist"` // exact, case-insensitive
	RawTitle    string  `json:"raw_title"`
	RawAlbum    string  `json:"raw_album"`
	RecordingID *int64  `json:"recording_id"`
	ReleaseID   *int64  `json:"release_id"`
	ArtistID    *int64  `json:"artist_id"` // via the recording's credits
	Unmatched   bool    `json:"unmatched"` // recording_id IS NULL
}

func (f Filter) where() (string, []any, error) {
	var w []string
	var a []any
	add := func(cond string, args ...any) { w = append(w, cond); a = append(a, args...) }
	if len(f.IDs) > 0 {
		add("id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.IDs)), ",")+")", idArgs(f.IDs)...)
	}
	if f.From != "" {
		t, err := time.Parse(time.RFC3339, f.From)
		if err != nil {
			return "", nil, invalid("bad_filter", "from must be RFC 3339")
		}
		add("played_at >= ?", t.UTC().Format(TimeFormat))
	}
	if f.To != "" {
		t, err := time.Parse(time.RFC3339, f.To)
		if err != nil {
			return "", nil, invalid("bad_filter", "to must be RFC 3339")
		}
		add("played_at < ?", t.UTC().Format(TimeFormat))
	}
	if f.Source != "" {
		add("source = ?", f.Source)
	}
	if f.RawArtist != "" {
		add("lower(raw_artist) = lower(?)", f.RawArtist)
	}
	if f.RawTitle != "" {
		add("lower(raw_title) = lower(?)", f.RawTitle)
	}
	if f.RawAlbum != "" {
		add("lower(raw_album) = lower(?)", f.RawAlbum)
	}
	if f.RecordingID != nil {
		add("recording_id = ?", *f.RecordingID)
	}
	if f.ReleaseID != nil {
		add("release_id = ?", *f.ReleaseID)
	}
	if f.ArtistID != nil {
		add("recording_id IN (SELECT recording_id FROM recording_artist WHERE artist_id = ?)", *f.ArtistID)
	}
	if f.Unmatched {
		add("recording_id IS NULL")
	}
	if len(w) == 0 {
		return "", nil, invalid("filter_required", "a bulk edit needs at least one filter criterion")
	}
	return strings.Join(w, " AND "), a, nil
}

func idArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, v := range ids {
		out[i] = v
	}
	return out
}

// BulkPreview is the dry run: how many scrobbles match, how many would
// actually change, and a few examples. Nothing is written.
type BulkPreview struct {
	Matched     int           `json:"matched"`
	WouldChange int           `json:"would_change"`
	Sample      []BulkExample `json:"sample"`
}

// BulkExample shows one planned scrobble change.
type BulkExample struct {
	ID     int64 `json:"id"`
	Before Row   `json:"before"`
	After  Row   `json:"after"`
}

func (e *Editor) bulkPlan(ctx context.Context, tx Querier, f Filter, set ScrobbleEdit) ([]Change, int, error) {
	if set.PlayedAt != nil {
		return nil, 0, invalid("bad_set", "bulk edits shift time with shift_seconds; played_at is single-scrobble only")
	}
	if set.NewRecording != nil || set.NewRelease != "" {
		return nil, 0, invalid("bad_set", "new_recording and new_release are single-scrobble only")
	}
	if !set.RecordingID.Set && !set.ReleaseID.Set && set.ShiftSeconds == nil {
		return nil, 0, invalid("bad_set", "nothing to set")
	}
	where, args, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	rows, err := queryRows(ctx, tx, `SELECT `+cols("scrobble")+` FROM scrobble WHERE `+where+` ORDER BY id LIMIT ?`, append(args, MaxBulk+1)...)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) > MaxBulk {
		return nil, 0, invalid("too_many", "filter matches more than %d scrobbles; narrow it", MaxBulk)
	}
	p := newPlanner(ctx, tx, e.now())
	var cs []Change
	for _, r := range rows {
		c, err := p.scrobble(r, set)
		if err != nil {
			return nil, 0, fmt.Errorf("%w", withScrobble(err, r["id"]))
		}
		cs = append(cs, c...)
	}
	return cs, len(rows), nil
}

func withScrobble(err error, id any) error {
	if e, ok := err.(*Error); ok {
		c := *e
		c.Msg = fmt.Sprintf("scrobble %v: %s", id, e.Msg)
		return &c
	}
	return err
}

// BulkDryRun plans a bulk edit and reports the counts without writing.
func (e *Editor) BulkDryRun(ctx context.Context, f Filter, set ScrobbleEdit) (*BulkPreview, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cs, matched, err := e.bulkPlan(ctx, tx, f, set)
	if err != nil {
		return nil, err
	}
	pv := &BulkPreview{Matched: matched, Sample: []BulkExample{}}
	for _, c := range cs {
		if c.Table != "scrobble" {
			continue
		}
		pv.WouldChange++
		if len(pv.Sample) < 20 {
			id, _ := asInt(c.Key["id"])
			pv.Sample = append(pv.Sample, BulkExample{ID: id, Before: c.Before, After: c.After})
		}
	}
	return pv, nil
}

// BulkApply applies a bulk edit as one log entry. expectedMatched must equal
// the dry run's matched count; if the data changed in between the edit is
// refused (409) and nothing is written.
func (e *Editor) BulkApply(ctx context.Context, actor string, f Filter, set ScrobbleEdit, expectedMatched int) (*Result, error) {
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		cs, matched, err := e.bulkPlan(ctx, tx, f, set)
		if err != nil {
			return nil, err
		}
		if matched != expectedMatched {
			return nil, &Error{Status: 409, Code: "count_changed",
				Msg: fmt.Sprintf("filter now matches %d scrobbles, not the %d you previewed; run the dry run again", matched, expectedMatched)}
		}
		return &draft{scope: "bulk", entityType: "scrobble", entityID: 0,
			summary: fmt.Sprintf("Bulk edit of %d scrobbles", matched), changes: cs}, nil
	})
}
