package edit_test

import (
	"fmt"
	"testing"

	"younithm/internal/edit"
	"younithm/internal/identity"
)

func aliasState(e *env, kind string, id int64) string {
	items, err := edit.Aliases(ctx, e.db, kind, id)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprint(items)
}

func TestAliasesToggleSearchOnlyAndUndo(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Reol")
	e.exec(`INSERT INTO alias(kind,raw_string,entity_id) VALUES ('artist','reol',?),('artist','れをる',?)`, a, a)
	e.exec(`INSERT INTO search_name(kind,entity_id,name,provider,provider_id) VALUES ('artist',?,'icy','musicbrainz','mb-reol')`, a)
	if got := aliasState(e, "artist", a); got != "[{れをる false } {icy true musicbrainz}]" {
		t.Fatalf("list %s (own name must be left out)", got)
	}
	before := e.snapshot()
	res, err := e.ed.SetAliasSearchOnly(ctx, "admin", "artist", a, "れをる", true)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE raw_string='れをる'`) != 0 || e.count(`SELECT COUNT(*) FROM search_name WHERE name='れをる'`) != 1 {
		t.Fatal("not moved to search only")
	}
	if _, err := e.ed.Undo(ctx, "admin", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != before || e.count(`SELECT COUNT(*) FROM alias WHERE raw_string='れをる'`) != 1 {
		t.Fatal("undo did not restore the matching alias")
	}
	if _, err := e.ed.SetAliasSearchOnly(ctx, "admin", "artist", a, "icy", false); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE raw_string='icy' AND entity_id=?`, a) != 1 {
		t.Fatal("search name did not become a matching alias")
	}
	wantErr(t, mustErr(e.ed.SetAliasSearchOnly(ctx, "admin", "artist", a, "icy", false)), "no_change")
	if e.count(`SELECT COUNT(*) FROM edit_log WHERE scope='alias'`) != 2 {
		t.Fatal("alias edits are not logged with the alias scope")
	}
}

func TestSearchNameAnotherArtistGoesByStaysSearchOnly(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Kobaryo")
	e.artist("Srezcat")
	e.exec(`INSERT INTO search_name(kind,entity_id,name,provider,provider_id) VALUES ('artist',?,'srezcat','musicbrainz','mb-k')`, a)
	wantErr(t, mustErr(e.ed.SetAliasSearchOnly(ctx, "admin", "artist", a, "srezcat", false)), "name_taken")
}

func TestRemovedAliasStaysRemoved(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Reol")
	e.exec(`INSERT INTO alias(kind,raw_string,entity_id) VALUES ('artist','145',?)`, a)
	res, err := e.ed.RemoveAlias(ctx, "admin", "artist", a, "145")
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE raw_string='145'`) != 0 || e.count(`SELECT COUNT(*) FROM alias_rejection WHERE entity_id=? AND name='145'`, a) != 1 {
		t.Fatal("not removed and remembered")
	}
	if added, err := identity.AddAlias(ctx, e.db, identity.KindArtist, a, "145"); err != nil || added {
		t.Fatalf("a removed alias was added back (%v)", err)
	}
	if _, err := e.ed.Undo(ctx, "admin", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE raw_string='145'`) != 1 || e.count(`SELECT COUNT(*) FROM alias_rejection`) != 0 {
		t.Fatal("undo did not bring the alias back")
	}
}

func TestMergeCarriesSearchNamesAndRemovals(t *testing.T) {
	e := newEnv(t)
	from, into := e.artist("Reol (dup)"), e.artist("Reol")
	e.exec(`INSERT INTO search_name(kind,entity_id,name) VALUES ('artist',?,'icy')`, from)
	e.exec(`INSERT INTO alias_rejection(kind,entity_id,name) VALUES ('artist',?,'145')`, from)
	if _, err := e.ed.Merge(ctx, "admin", "artist", from, into); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM search_name WHERE entity_id=? AND name='icy'`, into) != 1 ||
		e.count(`SELECT COUNT(*) FROM alias_rejection WHERE entity_id=? AND name='145'`, into) != 1 {
		t.Fatal("search names and removals did not follow the merge")
	}
}
