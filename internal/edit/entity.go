package edit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"younithm/internal/artwork"
)

var kindTable = map[string]string{KindArtist: "artist", KindRecording: "recording", KindRelease: "release"}

// editable fields per kind: name -> required non-empty string?
var editableFields = map[string]map[string]fieldType{
	KindArtist:    {"name": ftReq, "sort_name": ftStr, "display_name": ftStr},
	KindRecording: {"title": ftReq, "duration_ms": ftInt, "display_name": ftStr},
	KindRelease:   {"title": ftReq, "release_date": ftStr, "release_type": ftStr, "artwork_path": ftStr, "display_name": ftStr},
}

type fieldType int

const (
	ftReq fieldType = iota // non-empty string
	ftStr                  // string or null
	ftInt                  // integer or null
)

// EntityEdit sets fields on an artist, recording or release. Fields holds JSON
// values (null clears nullable fields). The old name is kept as an alias unless
// DropAlias is set, so future submissions of the old spelling still resolve.
type EntityEdit struct {
	Fields    map[string]any
	DropAlias bool
}

func (e *Editor) entityExists(ctx context.Context, tx Querier, kind string, id int64) (Row, error) {
	t, ok := kindTable[kind]
	if !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	row, found, err := readRow(ctx, tx, t, Row{"id": id}, tables[t].cols)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, notFound("%s %d not found", kind, id)
	}
	return row, nil
}

func nameField(kind string) string {
	if kind == KindArtist {
		return "name"
	}
	return "title"
}

// aliasKeys lists the alias strings under which entity id is (or was) known:
// its canonical name plus every raw spelling seen in scrobbles pointing at it.
func aliasKeys(ctx context.Context, tx Querier, kind string, id int64, row Row) ([]string, error) {
	keys, _, err := aliasNames(ctx, tx, kind, id, row)
	return keys, err
}

// aliasNames is aliasKeys with each key's spelling as written: a name, or a
// track's title.
func aliasNames(ctx context.Context, tx Querier, kind string, id int64, row Row) ([]string, map[string]string, error) {
	spell := map[string]string{}
	var out []string
	add := func(k, written string) {
		if k == "" || k == keySep {
			return
		}
		if _, seen := spell[k]; !seen {
			spell[k] = strings.TrimSpace(written)
			out = append(out, k)
		}
	}
	switch kind {
	case KindArtist:
		add(ArtistKey(str(row["name"])), str(row["name"]))
	case KindRecording:
		names, err := queryRows(ctx, tx, `SELECT a.name AS n FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
			WHERE ra.recording_id=? ORDER BY ra.position, a.id`, id)
		if err != nil {
			return nil, nil, err
		}
		title := str(row["title"])
		var all []string
		for _, n := range names {
			add(RecordingKey(str(n["n"]), title), title)
			all = append(all, str(n["n"]))
		}
		if len(all) > 1 {
			add(RecordingKey(strings.Join(all, ", "), title), title)
		}
		raws, err := queryRows(ctx, tx, `SELECT DISTINCT raw_artist AS a, raw_title AS t FROM scrobble WHERE recording_id=? LIMIT 1000`, id)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range raws {
			add(RecordingKey(str(r["a"]), str(r["t"])), str(r["t"]))
		}
	case KindRelease:
		add(ReleaseKey(str(row["title"])), str(row["title"]))
		raws, err := queryRows(ctx, tx, `SELECT DISTINCT raw_album AS a FROM scrobble WHERE release_id=? AND raw_album IS NOT NULL LIMIT 1000`, id)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range raws {
			add(ReleaseKey(str(r["a"])), str(r["a"]))
		}
	}
	return out, spell, nil
}

func str(v any) string { s, _ := v.(string); return s }

// aliasChanges plans alias rows key -> id. An existing alias for the same key
// is left alone when overwrite is false, otherwise repointed.
func aliasChanges(ctx context.Context, tx Querier, kind string, keys []string, spell map[string]string, id int64, overwrite bool) ([]Change, error) {
	var out []Change
	for _, k := range keys {
		rows, err := queryRows(ctx, tx, `SELECT id, entity_id FROM alias WHERE kind=? AND raw_string=?`, kind, k)
		if err != nil {
			return nil, err
		}
		switch {
		case len(rows) == 0:
			out = append(out, Change{Table: "alias", Key: nil, After: Row{"kind": kind, "raw_string": k, "entity_id": id, "display": spelling(spell[k])}})
		case overwrite && rows[0]["entity_id"] != id:
			out = append(out, Change{Table: "alias", Key: Row{"id": rows[0]["id"]},
				Before: Row{"entity_id": rows[0]["entity_id"]}, After: Row{"entity_id": id}})
		}
	}
	return out, nil
}

// EditEntity changes fields of an artist, recording or release for all plays
// at once. Scrobbles are not rewritten; they see the change through the model.
func (e *Editor) EditEntity(ctx context.Context, actor, kind string, id int64, ed EntityEdit) (*Result, error) {
	fields, ok := editableFields[kind]
	if !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, err := e.entityExists(ctx, tx, kind, id)
		if err != nil {
			return nil, err
		}
		before, after := Row{}, Row{}
		for name, raw := range ed.Fields {
			ft, ok := fields[name]
			if !ok {
				return nil, invalid("bad_field", "%s cannot be edited on a %s", name, kind)
			}
			var v any
			switch x := raw.(type) {
			case nil:
				if ft == ftReq {
					return nil, invalid("bad_value", "%s cannot be empty", name)
				}
			case string:
				x = strings.TrimSpace(x)
				if ft == ftInt {
					return nil, invalid("bad_value", "%s must be a number", name)
				}
				if x == "" && ft == ftReq {
					return nil, invalid("bad_value", "%s cannot be empty", name)
				}
				if name == "release_date" && x != "" && !validDate(x) {
					return nil, invalid("bad_value", "use a year, or a date like 2024-05 or 2024-05-17")
				}
				if x != "" {
					v = x
				}
			case float64:
				if ft != ftInt || x != float64(int64(x)) || x < 0 {
					return nil, invalid("bad_value", "%s must be a non-negative integer", name)
				}
				v = int64(x)
			default:
				return nil, invalid("bad_value", "%s has an unsupported type", name)
			}
			if canon(row[name]) != canon(v) {
				before[name], after[name] = row[name], v
			}
		}
		if len(after) == 0 {
			return nil, ErrNoChange
		}
		var cs []Change
		// Keep the old spelling resolving to this entity.
		if _, renamed := after[nameField(kind)]; renamed && !ed.DropAlias {
			keys, spell, err := aliasNames(ctx, tx, kind, id, row)
			if err != nil {
				return nil, err
			}
			ac, err := aliasChanges(ctx, tx, kind, keys, spell, id, false)
			if err != nil {
				return nil, err
			}
			cs = append(cs, ac...)
		}
		cs = append(cs, Change{Table: kindTable[kind], Key: Row{"id": id}, Before: before, After: after})
		return &draft{scope: "entity", entityType: kind, entityID: id,
			summary: fmt.Sprintf("Edit %s %d", kind, id), changes: cs}, nil
	})
}

// validDate accepts YYYY, YYYY-MM or YYYY-MM-DD with a real month and day.
func validDate(s string) bool {
	for _, layout := range []string{"2006", "2006-01", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil && t.Format(layout) == s {
			return true
		}
	}
	return false
}

// EditEntity is a helper to build an EntityEdit from a field map.
func EditEntity(fields map[string]any) EntityEdit { return EntityEdit{Fields: fields} }

// SetImage points an artist or album at an image chosen by hand (a file name
// under the artwork folder), with artwork.NoImage at none, or with name ""
// back to the fetched one. The fetched image is left alone, and the change
// undoes like any edit.
func (e *Editor) SetImage(ctx context.Context, actor, kind string, id int64, name string) (*Result, error) {
	if kind != KindArtist && kind != KindRelease {
		return nil, invalid("bad_kind", "images can be set for artists and albums")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		row, err := e.entityExists(ctx, tx, kind, id)
		if err != nil {
			return nil, err
		}
		var v any
		if name != "" {
			v = name
		}
		if canon(row["image_override"]) == canon(v) {
			return nil, ErrNoChange
		}
		summary := fmt.Sprintf("Set image of %s %d", kind, id)
		switch {
		case v == nil:
			summary = fmt.Sprintf("Reset image of %s %d", kind, id)
		case name == artwork.NoImage:
			summary = fmt.Sprintf("Remove image of %s %d", kind, id)
		}
		return &draft{scope: "entity", entityType: kind, entityID: id, summary: summary,
			changes: []Change{{Table: kindTable[kind], Key: Row{"id": id},
				Before: Row{"image_override": row["image_override"]}, After: Row{"image_override": v}}}}, nil
	})
}
