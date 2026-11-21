package edit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Error is a typed failure the HTTP layer maps to a status code.
type Error struct {
	Status    int // HTTP status
	Code      string
	Msg       string
	Conflicts []Conflict
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func invalid(code, msg string, a ...any) *Error {
	return &Error{Status: 400, Code: code, Msg: fmt.Sprintf(msg, a...)}
}
func notFound(msg string, a ...any) *Error {
	return &Error{Status: 404, Code: "not_found", Msg: fmt.Sprintf(msg, a...)}
}

// ErrNoChange is returned when an edit would change nothing.
var ErrNoChange = &Error{Status: 400, Code: "no_change", Msg: "edit changes nothing"}

// TimeFormat matches ingest.TimeFormat (scrobble.played_at).
const TimeFormat = "2006-01-02T15:04:05Z"

// Editor performs edits against the single write connection.
type Editor struct {
	DB  *sql.DB // write pool
	Now func() time.Time
}

func (e *Editor) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// draft is an edit computed inside a transaction, before it is applied.
type draft struct {
	scope      string
	entityType string
	entityID   int64
	summary    string
	changes    []Change
}

// Result is what every edit returns.
type Result struct {
	EditID  int64  `json:"edit_id"`
	Summary string `json:"summary"`
	Changes int    `json:"changes"`
}

// run executes plan in a transaction, applies its changes and logs them.
func (e *Editor) run(ctx context.Context, actor string, plan func(tx *sql.Tx) (*draft, error)) (*Result, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	d, err := plan(tx)
	if err != nil {
		return nil, err
	}
	if len(d.changes) == 0 {
		return nil, ErrNoChange
	}
	for i, c := range d.changes {
		done, err := apply(ctx, tx, c)
		if err != nil {
			return nil, err
		}
		d.changes[i] = done
	}
	id, err := e.log(ctx, tx, actor, d, 0)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{EditID: id, Summary: d.summary, Changes: len(d.changes)}, nil
}

func (e *Editor) log(ctx context.Context, tx *sql.Tx, actor string, d *draft, undoOf int64) (int64, error) {
	before, after, err := marshalChanges(d.changes)
	if err != nil {
		return 0, err
	}
	var undo any
	if undoOf != 0 {
		undo = undoOf
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO edit_log (at, actor, entity_type, entity_id, scope, before_json, after_json, summary, undo_of)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		e.now().Format("2006-01-02T15:04:05.000Z"), actor, d.entityType, d.entityID, d.scope, before, after, d.summary, undo)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Entry is one edit_log row. Before/After are admin-only raw data.
type Entry struct {
	ID         int64   `json:"id"`
	At         string  `json:"at"`
	Actor      string  `json:"actor"`
	Scope      string  `json:"scope"`
	EntityType string  `json:"entity_type"`
	EntityID   int64   `json:"entity_id"`
	Summary    string  `json:"summary"`
	UndoOf     *int64  `json:"undo_of,omitempty"`
	UndoneAt   *string `json:"undone_at,omitempty"`
	UndoneBy   *int64  `json:"undone_by,omitempty"`
	Changes    int     `json:"changes"`
	Before     any     `json:"before,omitempty"`
	After      any     `json:"after,omitempty"`
}

// ListFilter narrows List.
type ListFilter struct {
	EntityType string
	EntityID   int64
	Scope      string
	BeforeID   int64 // cursor: entries with id < BeforeID
	Limit      int
}

// List returns log entries newest first, without the before/after payloads.
func List(ctx context.Context, q Querier, f ListFilter) ([]Entry, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	where, args := "1=1", []any{}
	if f.EntityType != "" {
		where += " AND entity_type = ?"
		args = append(args, f.EntityType)
		if f.EntityID != 0 {
			where += " AND entity_id = ?"
			args = append(args, f.EntityID)
		}
	}
	if f.Scope != "" {
		where += " AND scope = ?"
		args = append(args, f.Scope)
	}
	if f.BeforeID != 0 {
		where += " AND id < ?"
		args = append(args, f.BeforeID)
	}
	args = append(args, f.Limit)
	rows, err := q.QueryContext(ctx, `SELECT id, at, actor, scope, entity_type, entity_id, summary, undo_of, undone_at, undone_by,
		json_array_length(after_json) FROM edit_log WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var en Entry
		var n sql.NullInt64
		if err := rows.Scan(&en.ID, &en.At, &en.Actor, &en.Scope, &en.EntityType, &en.EntityID, &en.Summary,
			&en.UndoOf, &en.UndoneAt, &en.UndoneBy, &n); err != nil {
			return nil, err
		}
		en.Changes = int(n.Int64)
		out = append(out, en)
	}
	return out, rows.Err()
}

// Get returns one entry with its before/after data.
func Get(ctx context.Context, q Querier, id int64) (*Entry, error) {
	en, cs, err := load(ctx, q, id)
	if err != nil {
		return nil, err
	}
	var b, a []wireChange
	for _, c := range cs {
		b = append(b, wireChange{c.Table, c.Key, c.Before})
		a = append(a, wireChange{c.Table, c.Key, c.After})
	}
	en.Before, en.After, en.Changes = b, a, len(cs)
	return en, nil
}

func load(ctx context.Context, q Querier, id int64) (*Entry, []Change, error) {
	var en Entry
	var before, after string
	err := q.QueryRowContext(ctx, `SELECT id, at, actor, scope, entity_type, entity_id, summary, undo_of, undone_at, undone_by,
		COALESCE(before_json,'[]'), COALESCE(after_json,'[]') FROM edit_log WHERE id = ?`, id).
		Scan(&en.ID, &en.At, &en.Actor, &en.Scope, &en.EntityType, &en.EntityID, &en.Summary, &en.UndoOf,
			&en.UndoneAt, &en.UndoneBy, &before, &after)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, notFound("edit %d not found", id)
	}
	if err != nil {
		return nil, nil, err
	}
	cs, err := unmarshalChanges(before, after)
	return &en, cs, err
}

// Undo reverts edit id. It refuses (409, with the conflicting columns) if any
// row the edit touched has been changed since, and refuses an edit that was
// already undone. Undoing an undo redoes the original.
func (e *Editor) Undo(ctx context.Context, actor string, id int64) (*Result, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	en, cs, err := load(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if en.UndoneAt != nil {
		return nil, &Error{Status: 409, Code: "already_undone", Msg: fmt.Sprintf("edit %d was already undone", id)}
	}
	inv := make([]Change, 0, len(cs))
	for i := len(cs) - 1; i >= 0; i-- {
		inv = append(inv, invert(cs[i]))
	}
	var conflicts []Conflict
	for _, c := range inv {
		cf, err := expectState(ctx, tx, c)
		if err != nil {
			return nil, err
		}
		conflicts = append(conflicts, cf...)
		if len(conflicts) >= 20 {
			break
		}
	}
	if len(conflicts) > 0 {
		return nil, &Error{Status: 409, Code: "undo_conflict",
			Msg: "later edits changed rows this edit touched; nothing was undone", Conflicts: conflicts}
	}
	for i, c := range inv {
		done, err := apply(ctx, tx, c)
		if err != nil {
			return nil, &Error{Status: 409, Code: "undo_conflict", Msg: err.Error()}
		}
		inv[i] = done
	}
	d := &draft{scope: "undo", entityType: en.EntityType, entityID: en.EntityID,
		summary: fmt.Sprintf("Undo #%d: %s", id, en.Summary), changes: inv}
	uid, err := e.log(ctx, tx, actor, d, id)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE edit_log SET undone_at=?, undone_by=? WHERE id=?`,
		e.now().Format("2006-01-02T15:04:05.000Z"), uid, id); err != nil {
		return nil, err
	}
	if en.Scope == "undo" && en.UndoOf != nil { // redo: the original is live again
		if _, err := tx.ExecContext(ctx, `UPDATE edit_log SET undone_at=NULL, undone_by=NULL WHERE id=?`, *en.UndoOf); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{EditID: uid, Summary: d.summary, Changes: len(inv)}, nil
}
