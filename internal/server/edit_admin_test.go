package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEditAdminEndpoints(t *testing.T) {
	e := newAdminEnv(t)
	const p = "/api/v1/admin"
	exec := func(q string, a ...any) int64 {
		res, err := e.pools.Write.Exec(q, a...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	art := exec(`INSERT INTO artist(name) VALUES ('Artist')`)
	recA := exec(`INSERT INTO recording(title) VALUES ('Sng')`)
	recB := exec(`INSERT INTO recording(title) VALUES ('Song')`)
	exec(`INSERT INTO recording_artist(recording_id, artist_id) VALUES (?,?)`, recA, art)
	exec(`INSERT INTO recording_artist(recording_id, artist_id) VALUES (?,?)`, recB, art)
	sc := exec(`INSERT INTO scrobble(played_at, raw_artist, raw_title, recording_id) VALUES ('2024-01-01T10:00:00Z','Artist','Sng',?)`, recA)

	// Everything is admin-only, including reads of history and raw values.
	for _, c := range [][2]string{
		{"GET", p + "/edits"}, {"GET", p + "/scrobbles/1"}, {"PATCH", p + "/scrobbles/1"},
		{"POST", p + "/scrobbles/bulk/preview"}, {"POST", p + "/scrobbles/bulk/apply"},
		{"POST", p + "/entities/recording/1/merge"}, {"POST", p + "/edits/1/undo"},
	} {
		if resp, _ := e.do(c[0], c[1], map[string]any{}); resp.StatusCode != 401 {
			t.Fatalf("%s %s unauthenticated: %d", c[0], c[1], resp.StatusCode)
		}
	}
	e.setup()

	// Single-scrobble edit, then raw-vs-effective view.
	resp, body := e.do("PATCH", fmt.Sprintf("%s/scrobbles/%d", p, sc), map[string]any{"played_at": "2024-02-02T00:00:00Z"})
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, body)
	}
	_, body = e.do("GET", fmt.Sprintf("%s/scrobbles/%d", p, sc), nil)
	if !strings.Contains(body, `"orig_played_at":"2024-01-01T10:00:00Z"`) || !strings.Contains(body, `"played_at":"2024-02-02T00:00:00Z"`) {
		t.Fatalf("view: %s", body)
	}
	resp, body = e.do("PATCH", fmt.Sprintf("%s/scrobbles/%d", p, sc), map[string]any{"recording_id": 9999})
	if resp.StatusCode != 400 || !strings.Contains(body, "unknown_recording") {
		t.Fatalf("bad recording: %d %s", resp.StatusCode, body)
	}

	// Merge needs step-up.
	mergePath := fmt.Sprintf("%s/entities/recording/%d/merge", p, recA)
	resp, body = e.do("POST", mergePath, map[string]any{"into": recB})
	if resp.StatusCode != 403 || !strings.Contains(body, "step_up_required") {
		t.Fatalf("merge without step-up: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do("POST", p+"/scrobbles/bulk/apply", map[string]any{}); resp.StatusCode != 403 {
		t.Fatalf("bulk apply without step-up: %d", resp.StatusCode)
	}
	// Bulk preview is read-only and needs only the session.
	resp, body = e.do("POST", p+"/scrobbles/bulk/preview", map[string]any{
		"filter": map[string]any{"recording_id": recA}, "set": map[string]any{"release_id": nil}})
	if resp.StatusCode != 200 && resp.StatusCode != 400 {
		t.Fatalf("preview: %d %s", resp.StatusCode, body)
	}

	if resp, body := e.do("POST", "/api/v1/auth/reauth", map[string]string{"password": adminPW}); resp.StatusCode != 200 {
		t.Fatalf("reauth: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", mergePath, map[string]any{"into": recB})
	if resp.StatusCode != 200 {
		t.Fatalf("merge: %d %s", resp.StatusCode, body)
	}
	var mr struct {
		EditID int64 `json:"edit_id"`
	}
	_ = json.Unmarshal([]byte(body), &mr)

	// Bulk: apply requires the previewed count.
	resp, body = e.do("POST", p+"/scrobbles/bulk/apply", map[string]any{
		"filter": map[string]any{"recording_id": recB}, "set": map[string]any{"shift_seconds": 60}})
	if resp.StatusCode != 400 || !strings.Contains(body, "preview_required") {
		t.Fatalf("apply without preview: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", p+"/scrobbles/bulk/preview", map[string]any{
		"filter": map[string]any{"recording_id": recB}, "set": map[string]any{"shift_seconds": 60}})
	if resp.StatusCode != 200 || !strings.Contains(body, `"matched":1`) {
		t.Fatalf("preview: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", p+"/scrobbles/bulk/apply", map[string]any{
		"filter": map[string]any{"recording_id": recB}, "set": map[string]any{"shift_seconds": 60}, "expected_matched": 1})
	if resp.StatusCode != 200 {
		t.Fatalf("apply: %d %s", resp.StatusCode, body)
	}

	// History is listed with raw before/after on the detail endpoint only.
	_, body = e.do("GET", p+"/edits", nil)
	if !strings.Contains(body, "Merge recording") || strings.Contains(body, `"before"`) {
		t.Fatalf("list: %s", body)
	}
	_, body = e.do("GET", fmt.Sprintf("%s/edits/%d", p, mr.EditID), nil)
	if !strings.Contains(body, `"before"`) || !strings.Contains(body, `"after"`) {
		t.Fatalf("detail: %s", body)
	}

	// Conflicts are per column: the bulk edit only shifted time, so the merge is
	// still undoable. Unlinking the scrobble afterwards is a real conflict.
	if resp, body := e.do("PATCH", fmt.Sprintf("%s/scrobbles/%d", p, sc), map[string]any{"recording_id": nil}); resp.StatusCode != 200 {
		t.Fatalf("unlink: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", fmt.Sprintf("%s/edits/%d/undo", p, mr.EditID), nil)
	if resp.StatusCode != 409 || !strings.Contains(body, "undo_conflict") || !strings.Contains(body, `"conflicts"`) {
		t.Fatalf("undo merge: %d %s", resp.StatusCode, body)
	}
}
