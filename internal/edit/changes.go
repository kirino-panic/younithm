// Package edit implements editing (docs/editing.md): editing scrobbles and
// entities, merges with aliases, bulk edits, album membership, and an
// edit_log with undo.
//
// Every edit is expressed as a list of row-level Changes. The edit and its
// log entry are written in one transaction, and undo is the same list applied
// in reverse with before/after swapped, after checking that the rows still
// look the way the edit left them. Scrobble raw_* columns are never part of
// any change, so raw values cannot be lost.
package edit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Row is a set of column values.
type Row map[string]any

// Change is one row-level change. Before == nil means the row was inserted,
// After == nil means it was deleted; otherwise it is an update and both hold
// only the changed columns. Key identifies the row by primary key.
type Change struct {
	Table  string
	Key    Row
	Before Row
	After  Row
}

type tableSpec struct {
	pk   []string
	cols []string // every column, for full-row insert/delete snapshots
}

// tables is the allowlist of tables and columns edits may touch; SQL
// identifiers are only ever taken from here.
var tables = map[string]tableSpec{
	"scrobble": {[]string{"id"}, []string{"id", "user_id", "played_at", "duration_played_ms", "source",
		"recording_id", "release_id", "raw_artist", "raw_title", "raw_album", "raw_payload", "created_at",
		"orig_played_at", "edited_at"}},
	"artist":           {[]string{"id"}, []string{"id", "name", "sort_name", "created_at", "verified", "unsplit", "display_name", "image_override"}},
	"recording":        {[]string{"id"}, []string{"id", "title", "duration_ms", "work_id", "created_at", "fuzzy_consent", "display_name"}},
	"release":          {[]string{"id"}, []string{"id", "title", "release_date", "release_type", "artwork_path", "created_at", "release_group_mbid", "display_name", "image_override"}},
	"recording_artist": {[]string{"recording_id", "artist_id", "role"}, []string{"recording_id", "artist_id", "role", "position", "join_phrase", "credited_name"}},
	"release_artist":   {[]string{"release_id", "artist_id"}, []string{"release_id", "artist_id", "position", "join_phrase", "credited_name"}},
	"recording_release": {[]string{"recording_id", "release_id"},
		[]string{"recording_id", "release_id", "disc", "position", "is_primary"}},
	"alias":        {[]string{"id"}, []string{"id", "kind", "raw_string", "entity_id"}},
	"artist_split": {[]string{"raw_string", "position"}, []string{"raw_string", "position", "artist_id", "join_phrase"}},
	"external_link": {[]string{"id"}, []string{"id", "entity_type", "entity_id", "provider", "provider_id",
		"match_method", "confidence", "status", "pinned", "created_at"}},
	"link_rejection": {[]string{"entity_type", "entity_id", "provider", "provider_id"},
		[]string{"entity_type", "entity_id", "provider", "provider_id", "created_at"}},
}

// Querier is satisfied by *sql.DB and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// queryRows runs q and returns each row keyed by column name.
func queryRows(ctx context.Context, tx Querier, q string, args ...any) ([]Row, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func keyWhere(spec tableSpec, key Row) (string, []any, error) {
	var parts []string
	var args []any
	for _, c := range spec.pk {
		v, ok := key[c]
		if !ok {
			return "", nil, fmt.Errorf("edit: missing key column %s", c)
		}
		parts = append(parts, c+" = ?")
		args = append(args, v)
	}
	return strings.Join(parts, " AND "), args, nil
}

func pickKey(spec tableSpec, row Row) Row {
	k := Row{}
	for _, c := range spec.pk {
		k[c] = row[c]
	}
	return k
}

// readRow loads the named columns of one row; ok is false if it is absent.
func readRow(ctx context.Context, tx Querier, table string, key Row, cols []string) (Row, bool, error) {
	spec, ok := tables[table]
	if !ok {
		return nil, false, fmt.Errorf("edit: unknown table %q", table)
	}
	where, args, err := keyWhere(spec, key)
	if err != nil {
		return nil, false, err
	}
	rows, err := queryRows(ctx, tx, "SELECT "+strings.Join(cols, ", ")+" FROM "+table+" WHERE "+where, args...)
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}

func sortedKeys(r Row) []string {
	ks := make([]string, 0, len(r))
	for k := range r {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func validCols(spec tableSpec, r Row) error {
	for c := range r {
		found := false
		for _, s := range spec.cols {
			if s == c {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("edit: unknown column %q", c)
		}
	}
	return nil
}

// apply executes one change. For an insert into a table keyed by an
// auto-assigned id, the returned change carries that id so it can be undone.
func apply(ctx context.Context, tx Querier, c Change) (Change, error) {
	return c, applyInner(ctx, tx, &c)
}

func applyInner(ctx context.Context, tx Querier, cp *Change) error {
	c := *cp
	spec, ok := tables[c.Table]
	if !ok {
		return fmt.Errorf("edit: unknown table %q", c.Table)
	}
	switch {
	case c.Before == nil && c.After == nil:
		return fmt.Errorf("edit: empty change")
	case c.Before == nil: // insert
		if err := validCols(spec, c.After); err != nil {
			return err
		}
		ks := sortedKeys(c.After)
		args := make([]any, len(ks))
		for i, k := range ks {
			args[i] = c.After[k]
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO "+c.Table+" ("+strings.Join(ks, ",")+") VALUES ("+
			strings.TrimSuffix(strings.Repeat("?,", len(ks)), ",")+")", args...)
		if err != nil {
			return err
		}
		if len(spec.pk) == 1 && c.After[spec.pk[0]] == nil {
			id, _ := res.LastInsertId()
			c2 := Row{spec.pk[0]: id}
			for k, v := range c.After {
				c2[k] = v
			}
			cp.After, cp.Key = c2, Row{spec.pk[0]: id}
		}
		return nil
	case c.After == nil: // delete
		where, args, err := keyWhere(spec, c.Key)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM "+c.Table+" WHERE "+where, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("edit: delete %s %v: row missing", c.Table, c.Key)
		}
		return nil
	default: // update
		if err := validCols(spec, c.After); err != nil {
			return err
		}
		ks := sortedKeys(c.After)
		sets := make([]string, len(ks))
		var args []any
		for i, k := range ks {
			sets[i] = k + " = ?"
			args = append(args, c.After[k])
		}
		where, kargs, err := keyWhere(spec, c.Key)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE "+c.Table+" SET "+strings.Join(sets, ", ")+" WHERE "+where, append(args, kargs...)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("edit: update %s %v: row missing", c.Table, c.Key)
		}
		return nil
	}
}

func invert(c Change) Change {
	return Change{Table: c.Table, Key: c.Key, Before: c.After, After: c.Before}
}

func canon(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Conflict says why a change could not be undone.
type Conflict struct {
	Table    string `json:"table"`
	Key      Row    `json:"key"`
	Column   string `json:"column,omitempty"`
	Expected any    `json:"expected"`
	Actual   any    `json:"actual"`
}

// expectState reports where the current row differs from what the change
// expects to find (c.Before).
func expectState(ctx context.Context, tx Querier, c Change) ([]Conflict, error) {
	spec := tables[c.Table]
	if c.Before == nil { // expect the row to be absent
		_, ok, err := readRow(ctx, tx, c.Table, c.Key, spec.pk)
		if err != nil {
			return nil, err
		}
		if ok {
			return []Conflict{{Table: c.Table, Key: c.Key, Expected: "absent", Actual: "present"}}, nil
		}
		return nil, nil
	}
	cols := sortedKeys(c.Before)
	cur, ok, err := readRow(ctx, tx, c.Table, c.Key, cols)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Conflict{{Table: c.Table, Key: c.Key, Expected: "present", Actual: "absent"}}, nil
	}
	var out []Conflict
	for _, col := range cols {
		if canon(cur[col]) != canon(c.Before[col]) {
			out = append(out, Conflict{Table: c.Table, Key: c.Key, Column: col, Expected: c.Before[col], Actual: cur[col]})
		}
	}
	return out, nil
}

type wireChange struct {
	T string `json:"t"`
	K Row    `json:"k"`
	R Row    `json:"r"`
}

func marshalChanges(cs []Change) (before, after string, err error) {
	b := make([]wireChange, len(cs))
	a := make([]wireChange, len(cs))
	for i, c := range cs {
		b[i] = wireChange{c.Table, c.Key, c.Before}
		a[i] = wireChange{c.Table, c.Key, c.After}
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return "", "", err
	}
	ab, err := json.Marshal(a)
	if err != nil {
		return "", "", err
	}
	return string(bb), string(ab), nil
}

func unmarshalChanges(before, after string) ([]Change, error) {
	var b, a []wireChange
	if err := json.Unmarshal([]byte(before), &b); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(after), &a); err != nil {
		return nil, err
	}
	if len(a) != len(b) {
		return nil, fmt.Errorf("edit: corrupt log entry")
	}
	out := make([]Change, len(b))
	for i := range b {
		out[i] = Change{Table: b[i].T, Key: intify(b[i].K), Before: intify(b[i].R), After: intify(a[i].R)}
	}
	return out, nil
}

// intify turns whole float64s (what encoding/json yields) back into int64 so
// they bind as integers.
func intify(r Row) Row {
	if r == nil {
		return nil
	}
	for k, v := range r {
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			r[k] = int64(f)
		}
	}
	return r
}
