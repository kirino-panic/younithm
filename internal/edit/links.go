package edit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Catalogue links a person sets by hand. A link set here is confirmed and
// pinned, so matching never replaces it; the ids it replaces (and a link
// removed here) are recorded as rejections, so matching never proposes them
// again.

var linkProviders = map[string]bool{"musicbrainz": true, "spotify": true, "apple": true}

func checkLinkTarget(kind, provider string) error {
	if _, ok := kindTable[kind]; !ok {
		return invalid("bad_kind", "kind must be artist, recording or release")
	}
	if !linkProviders[provider] {
		return invalid("bad_provider", "provider must be musicbrainz, spotify or apple")
	}
	return nil
}

func rejection(kind string, id int64, provider, pid string) Row {
	return Row{"entity_type": kind, "entity_id": id, "provider": provider, "provider_id": pid}
}

// SetLink makes providerID the entity's link for provider.
func (e *Editor) SetLink(ctx context.Context, actor, kind string, id int64, provider, providerID string) (*Result, error) {
	providerID = strings.TrimSpace(providerID)
	if err := checkLinkTarget(kind, provider); err != nil {
		return nil, err
	}
	if providerID == "" {
		return nil, invalid("bad_value", "provider id is empty")
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		if _, err := e.entityExists(ctx, tx, kind, id); err != nil {
			return nil, err
		}
		links, err := queryRows(ctx, tx, `SELECT `+cols("external_link")+` FROM external_link
WHERE entity_type=? AND entity_id=? AND provider=?`, kind, id, provider)
		if err != nil {
			return nil, err
		}
		var cs []Change
		var same Row
		for _, l := range links {
			if l["provider_id"] == providerID {
				same = l
			}
		}
		for _, l := range links {
			if l["provider_id"] == providerID {
				continue
			}
			rc, err := addRejection(ctx, tx, kind, id, provider, str(l["provider_id"]))
			if err != nil {
				return nil, err
			}
			cs = append(cs, rc...)
			if same == nil {
				// The first replaced row becomes the new link (rows are updated in
				// place, so undo sees the ids it expects).
				same = Row{}
				for k, v := range l {
					same[k] = v
				}
				cs = append(cs, Change{Table: "external_link", Key: Row{"id": l["id"]},
					Before: Row{"provider_id": l["provider_id"]}, After: Row{"provider_id": providerID}})
				same["provider_id"] = providerID
				continue
			}
			cs = append(cs, Change{Table: "external_link", Key: Row{"id": l["id"]}, Before: l})
		}
		// A rejection of this id (an earlier "none of these") no longer holds.
		key := rejection(kind, id, provider, providerID)
		if old, ok, err := readRow(ctx, tx, "link_rejection", key, tables["link_rejection"].cols); err != nil {
			return nil, err
		} else if ok {
			cs = append(cs, Change{Table: "link_rejection", Key: key, Before: old})
		}
		want := Row{"match_method": "manual", "confidence": float64(1), "status": "confirmed", "pinned": int64(1)}
		if same != nil {
			before, after := Row{}, Row{}
			for k, v := range want {
				if canon(same[k]) != canon(v) {
					before[k], after[k] = same[k], v
				}
			}
			if len(after) > 0 {
				cs = append(cs, Change{Table: "external_link", Key: Row{"id": same["id"]}, Before: before, After: after})
			}
		} else {
			n := Row{"entity_type": kind, "entity_id": id, "provider": provider, "provider_id": providerID}
			for k, v := range want {
				n[k] = v
			}
			cs = append(cs, Change{Table: "external_link", After: n})
		}
		if len(cs) == 0 {
			return nil, ErrNoChange
		}
		return &draft{scope: "link", entityType: kind, entityID: id,
			summary: fmt.Sprintf("Link %s %d to %s %s", kind, id, provider, providerID), changes: cs}, nil
	})
}

// RemoveLink drops the entity's links to provider and keeps matching from
// adding them back.
func (e *Editor) RemoveLink(ctx context.Context, actor, kind string, id int64, provider string) (*Result, error) {
	if err := checkLinkTarget(kind, provider); err != nil {
		return nil, err
	}
	return e.run(ctx, actor, func(tx *sql.Tx) (*draft, error) {
		if _, err := e.entityExists(ctx, tx, kind, id); err != nil {
			return nil, err
		}
		links, err := queryRows(ctx, tx, `SELECT `+cols("external_link")+` FROM external_link
WHERE entity_type=? AND entity_id=? AND provider=?`, kind, id, provider)
		if err != nil {
			return nil, err
		}
		if len(links) == 0 {
			return nil, ErrNoChange
		}
		var cs []Change
		for _, l := range links {
			cs = append(cs, Change{Table: "external_link", Key: Row{"id": l["id"]}, Before: l})
			rc, err := addRejection(ctx, tx, kind, id, provider, str(l["provider_id"]))
			if err != nil {
				return nil, err
			}
			cs = append(cs, rc...)
		}
		return &draft{scope: "link", entityType: kind, entityID: id,
			summary: fmt.Sprintf("Unlink %s %d from %s", kind, id, provider), changes: cs}, nil
	})
}

func addRejection(ctx context.Context, tx Querier, kind string, id int64, provider, pid string) ([]Change, error) {
	key := rejection(kind, id, provider, pid)
	if _, ok, err := readRow(ctx, tx, "link_rejection", key, []string{"provider_id"}); err != nil || ok {
		return nil, err
	}
	return []Change{{Table: "link_rejection", Key: key, After: key}}, nil
}
