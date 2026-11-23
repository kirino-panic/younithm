package edit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AliasEntry is one other name of an entity, as the edit panel lists it. Name
// is the normalised key (a recording's carries its artists, joined to the
// title by a unit separator). A search-only name finds the entity in search
// and is never used to match a listen.
type AliasEntry struct {
	Name       string `json:"name"`
	SearchOnly bool   `json:"search_only"`
	Source     string `json:"source,omitempty"` // the catalogue it was read from
}

// Aliases lists an entity's other names, matching ones first. The keys of its
// own current name are left out.
func Aliases(ctx context.Context, q Querier, kind string, id int64) ([]AliasEntry, error) {
	row, err := (&Editor{}).entityExists(ctx, q, kind, id)
	if err != nil {
		return nil, err
	}
	own, err := aliasKeys(ctx, q, kind, id, row)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, k := range own {
		skip[k] = true
	}
	rows, err := q.QueryContext(ctx, `
SELECT raw_string, 0, '' FROM alias WHERE kind=?1 AND entity_id=?2
UNION ALL SELECT name, 1, provider FROM search_name WHERE kind=?1 AND entity_id=?2
ORDER BY 2, 1`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AliasEntry{}
	for rows.Next() {
		var a AliasEntry
		if err := rows.Scan(&a.Name, &a.SearchOnly, &a.Source); err != nil {
			return nil, err
		}
		if !skip[a.Name] {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

func aliasRow(ctx context.Context, tx Querier, kind string, id int64, name string) (alias, search Row, err error) {
	if rs, err := queryRows(ctx, tx, `SELECT `+cols("alias")+` FROM alias WHERE kind=? AND entity_id=? AND raw_string=?`, kind, id, name); err != nil {
		return nil, nil, err
	} else if len(rs) > 0 {
		alias = rs[0]
	}
	if rs, err := queryRows(ctx, tx, `SELECT `+cols("search_name")+` FROM search_name WHERE kind=? AND entity_id=? AND name=?`, kind, id, name); err != nil {
		return nil, nil, err
	} else if len(rs) > 0 {
		search = rs[0]
	}
	return alias, search, nil
}

// SetAliasSearchOnly makes one of an entity's other names search-only (on), or
// a name listens are matched by (off). A name another entry already goes by,
// or holds as a matching alias, cannot match listens to this one too.
func (e *Editor) SetAliasSearchOnly(ctx context.Context, actor, kind string, id int64, name string, on bool) (*Result, error) {
	if _, ok := kindTable[kind]; !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		if _, err := e.entityExists(ctx, tx, kind, id); err != nil {
			return nil, err
		}
		al, sn, err := aliasRow(ctx, tx, kind, id, name)
		if err != nil {
			return nil, err
		}
		var cs []Change
		switch {
		case on && al != nil:
			cs = append(cs, Change{Table: "alias", Key: Row{"id": al["id"]}, Before: al})
			if sn == nil {
				cs = append(cs, Change{Table: "search_name", After: Row{"kind": kind, "entity_id": id, "name": name, "provider": "", "provider_id": ""}})
			}
		case !on && sn != nil:
			if taken, err := nameTaken(ctx, tx, kind, id, name); err != nil {
				return nil, err
			} else if taken {
				return nil, invalid("name_taken", "another %s already goes by this name, so it can only be used in search", kindLabel(kind))
			}
			cs = append(cs, Change{Table: "search_name", Key: Row{"id": sn["id"]}, Before: sn})
			if al == nil {
				cs = append(cs, Change{Table: "alias", After: Row{"kind": kind, "raw_string": name, "entity_id": id}})
			}
		case al == nil && sn == nil:
			return nil, notFound("%s %d has no name %q", kind, id, name)
		default:
			return nil, ErrNoChange
		}
		what := "matching"
		if on {
			what = "search only"
		}
		return &draft{scope: "alias", entityType: kind, entityID: id,
			summary: fmt.Sprintf("Make name of %s %d %s", kind, id, what), changes: cs}, nil
	})
}

// RemoveAlias removes one of an entity's other names, matching or search-only,
// and remembers it was removed: no catalogue or listen adds it back.
func (e *Editor) RemoveAlias(ctx context.Context, actor, kind string, id int64, name string) (*Result, error) {
	if _, ok := kindTable[kind]; !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		if _, err := e.entityExists(ctx, tx, kind, id); err != nil {
			return nil, err
		}
		al, sn, err := aliasRow(ctx, tx, kind, id, name)
		if err != nil {
			return nil, err
		}
		if al == nil && sn == nil {
			return nil, notFound("%s %d has no name %q", kind, id, name)
		}
		var cs []Change
		if al != nil {
			cs = append(cs, Change{Table: "alias", Key: Row{"id": al["id"]}, Before: al})
		}
		if sn != nil {
			cs = append(cs, Change{Table: "search_name", Key: Row{"id": sn["id"]}, Before: sn})
		}
		key := Row{"kind": kind, "entity_id": id, "name": name}
		if rs, err := queryRows(ctx, tx, `SELECT 1 FROM alias_rejection WHERE kind=? AND entity_id=? AND name=?`, kind, id, name); err != nil {
			return nil, err
		} else if len(rs) == 0 {
			cs = append(cs, Change{Table: "alias_rejection", Key: key, After: Row{"kind": kind, "entity_id": id, "name": name}})
		}
		return &draft{scope: "alias", entityType: kind, entityID: id,
			summary: fmt.Sprintf("Remove a name of %s %d", kind, id), changes: cs}, nil
	})
}

// nameTaken reports whether a key is another entity's matching alias or the
// key of another entity's own name.
func nameTaken(ctx context.Context, tx Querier, kind string, id int64, key string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM alias WHERE kind=? AND raw_string=? AND entity_id<>?`, kind, key, id).Scan(&n); err != nil || n > 0 {
		return n > 0, err
	}
	var q string
	switch kind {
	case KindArtist:
		q = `SELECT COUNT(*) FROM artist WHERE id<>?1 AND lower(trim(name))=?2`
	case KindRelease:
		q = `SELECT COUNT(*) FROM release WHERE id<>?1 AND lower(trim(title))=?2`
	default:
		q = `SELECT COUNT(*) FROM recording r JOIN recording_artist ra ON ra.recording_id=r.id JOIN artist a ON a.id=ra.artist_id
			WHERE r.id<>?1 AND lower(trim(a.name)) || char(31) || lower(trim(r.title)) = ?2`
	}
	err := tx.QueryRowContext(ctx, q, id, strings.TrimSpace(key)).Scan(&n)
	return n > 0, err
}
