package edit_test

import (
	"testing"

	"younithm/internal/edit"
)

func TestReleaseDateTakesAYearMonthOrDay(t *testing.T) {
	e := newEnv(t)
	rel := e.release("Album", "2020")
	for _, ok := range []string{"1999", "1999-07", "1999-07-31"} {
		if _, err := e.ed.EditEntity(ctx, "admin", "release", rel, edit.EditEntity(map[string]any{"release_date": ok})); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"99", "1999-13", "1999-02-30", "July 1999", "1999-7"} {
		wantErr(t, mustErr(e.ed.EditEntity(ctx, "admin", "release", rel, edit.EditEntity(map[string]any{"release_date": bad}))), "bad_value")
	}
	if _, err := e.ed.EditEntity(ctx, "admin", "release", rel, edit.EditEntity(map[string]any{"release_date": nil})); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM release WHERE id=? AND release_date IS NULL`, rel) != 1 {
		t.Fatal("clearing the date did not clear it")
	}
}
