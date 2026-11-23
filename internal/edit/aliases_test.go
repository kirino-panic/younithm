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
	if got := aliasState(e, "artist", a); got != "[{れをる false  } {icy true musicbrainz }]" {
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
	if added, err := identity.AddAlias(ctx, e.db, identity.KindArtist, a, "145", "145"); err != nil || added {
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

func TestAddAliasByHand(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Reol")
	e.artist("Srezcat")
	e.exec(`INSERT INTO alias_rejection(kind,entity_id,name) VALUES ('artist',?,'icy')`, a)

	res, so, err := e.ed.AddAlias(ctx, "admin", "artist", a, "  れをる ", false)
	if err != nil || so {
		t.Fatalf("add: %v search_only=%v", err, so)
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE kind='artist' AND raw_string='れをる' AND entity_id=?`, a) != 1 {
		t.Fatal("matching name not added")
	}
	if _, err := e.ed.Undo(ctx, "admin", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE raw_string='れをる'`) != 0 {
		t.Fatal("undo left the name")
	}

	// Another artist's name can only be added for search.
	if _, so, err := e.ed.AddAlias(ctx, "admin", "artist", a, "Srezcat", false); err != nil || !so {
		t.Fatalf("taken name: %v search_only=%v", err, so)
	}
	// Adding a removed name lifts the removal.
	if _, _, err := e.ed.AddAlias(ctx, "admin", "artist", a, "icy", true); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM alias_rejection`) != 0 || e.count(`SELECT COUNT(*) FROM search_name WHERE name='icy'`) != 1 {
		t.Fatal("removal not lifted")
	}
	_, _, err = e.ed.AddAlias(ctx, "admin", "artist", a, "ICY", false)
	wantErr(t, err, "name_listed")
	_, _, err = e.ed.AddAlias(ctx, "admin", "artist", a, "reol", false)
	wantErr(t, err, "own_name")
	_, _, err = e.ed.AddAlias(ctx, "admin", "artist", a, " ", false)
	wantErr(t, err, "empty_name")
}

func TestAddTrackNameUsesFirstArtist(t *testing.T) {
	e := newEnv(t)
	a, b := e.artist("Reol"), e.artist("Giga")
	r := e.recording("Gekihaku", a, b)
	if _, _, err := e.ed.AddAlias(ctx, "admin", "recording", r, "激白", false); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE kind='recording' AND raw_string=? AND entity_id=?`, identity.RecordingAliasKey("Reol", "激白"), r) != 1 {
		t.Fatal("track name not keyed with its first artist")
	}
}

func TestNameSpellingIsKeptAndEditable(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Reol")
	if _, _, err := e.ed.AddAlias(ctx, "admin", "artist", a, "Re Woru", true); err != nil {
		t.Fatal(err)
	}
	if got := aliasState(e, "artist", a); got != "[{re woru true  Re Woru}]" {
		t.Fatalf("typed spelling not kept: %s", got)
	}
	// Moving between matching and search-only keeps the spelling.
	if _, err := e.ed.SetAliasSearchOnly(ctx, "admin", "artist", a, "re woru", false); err != nil {
		t.Fatal(err)
	}
	if got := aliasState(e, "artist", a); got != "[{re woru false  Re Woru}]" {
		t.Fatalf("spelling lost on switch: %s", got)
	}
	res, err := e.ed.SetAliasSpelling(ctx, "admin", "artist", a, "re woru", "  RE  woru ")
	if err != nil {
		t.Fatal(err)
	}
	if got := aliasState(e, "artist", a); got != "[{re woru false  RE  woru}]" {
		t.Fatalf("respelled: %s", got)
	}
	if _, err := e.ed.Undo(ctx, "admin", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := aliasState(e, "artist", a); got != "[{re woru false  Re Woru}]" {
		t.Fatalf("undo: %s", got)
	}
	wantErr(t, mustErr(e.ed.SetAliasSpelling(ctx, "admin", "artist", a, "re woru", "Reworu")), "not_same_name")
	wantErr(t, mustErr(e.ed.SetAliasSpelling(ctx, "admin", "artist", a, "re woru", "Re Woru")), "no_change")

	// A track's spelling is its title.
	r := e.recording("Gekihaku", a)
	if _, _, err := e.ed.AddAlias(ctx, "admin", "recording", r, "激白", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.SetAliasSpelling(ctx, "admin", "recording", r, identity.RecordingAliasKey("Reol", "gekihaku remix"), "Gekihaku Remix"); err == nil {
		t.Fatal("respelled a name the track doesn't have")
	}
	key := identity.RecordingAliasKey("Reol", "the title")
	e.exec(`INSERT INTO alias(kind,raw_string,entity_id) VALUES ('recording',?,?)`, key, r)
	if _, err := e.ed.SetAliasSpelling(ctx, "admin", "recording", r, key, "The Title"); err != nil {
		t.Fatal(err)
	}
}

func TestListensFillMissingSpellingOnly(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Reol")
	e.exec(`INSERT INTO alias(kind,raw_string,entity_id) VALUES ('artist','れをる',?)`, a)
	if _, err := identity.AddAlias(ctx, e.db, identity.KindArtist, a, "れをる", "れをる"); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO alias(kind,raw_string,entity_id,display) VALUES ('artist','rei',?,'REI')`, a)
	if _, err := identity.AddAlias(ctx, e.db, identity.KindArtist, a, "rei", "Rei"); err != nil {
		t.Fatal(err)
	}
	if got := aliasState(e, "artist", a); got != "[{rei false  REI} {れをる false  れをる}]" {
		t.Fatalf("%s", got)
	}
}
