package enrich

import (
	"context"
	"strings"
	"testing"
)

func TestFillTrackPositions(t *testing.T) {
	e := setup(t)
	exec(t, e.db, `INSERT INTO release(id,title) VALUES (1,'Album')`)
	exec(t, e.db, `INSERT INTO recording_release(recording_id,release_id) VALUES (1,1)`)
	exec(t, e.db, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status)
		VALUES ('recording',1,'spotify','t1','isrc','auto'), ('release',1,'spotify','al1','lookup','auto')`)
	exec(t, e.db, `INSERT INTO provider_snapshot(provider,provider_id,kind,fetched_at,raw_json)
		VALUES ('spotify','t1','recording','2026-01-01T00:00:00Z','{"Releases":[{"ProviderID":"other","Position":9},{"ProviderID":"al1","Disc":2,"Position":4}]}')`)

	n, err := e.m.fillTrackPositions(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("filled %d, %v; want 1", n, err)
	}
	var disc, pos int
	if err := e.db.QueryRow(`SELECT disc, position FROM recording_release WHERE recording_id=1 AND release_id=1`).Scan(&disc, &pos); err != nil {
		t.Fatal(err)
	}
	if disc != 2 || pos != 4 {
		t.Fatalf("disc %d position %d, want 2 and 4", disc, pos)
	}
}

// The pass runs on the only write connection, so it must start from the
// tracks missing a position, not pair every track link with every album link.
func TestFillTrackPositionsPlan(t *testing.T) {
	e := setup(t)
	rows, err := e.db.Query(`EXPLAIN QUERY PLAN ` + fillPositionsSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	got := strings.Join(plan, "\n")
	if !strings.Contains(got, "SCAN rr") || strings.Contains(got, "external_link_lookup") {
		t.Fatalf("plan does not start from recording_release:\n%s", got)
	}
}
