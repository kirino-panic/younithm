package imports

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckFile(t *testing.T) {
	m := &Manager{}
	check := func(body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		m.checkFile(w, httptest.NewRequest("POST", "/admin/imports/check", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	head := func(s string) string {
		b, _ := json.Marshal(map[string]string{"head": base64.StdEncoding.EncodeToString([]byte(s))})
		return string(b)
	}

	if code, out := check(head(`[{"ts":"2020","ms_played":1000,"master_metadata_track_name":"x"}]`)); code != 200 || out["format"] != "spotify" {
		t.Fatalf("spotify: %d %v", code, out)
	}
	if code, out := check(head("Track Identifier,Date Played,Play Duration Milliseconds\n1,2,3\n")); code != 200 || out["format"] != "applemusic" || out["roles"].([]any)[0] != "daily_tracks" {
		t.Fatalf("apple daily tracks: %d %v", code, out)
	}
	if code, out := check(head("hello there")); code != 422 || out["error"] != "unknown_format" {
		t.Fatalf("unknown: %d %v", code, out)
	}
	code, out := check(`{"entries":["Apple_Media_Services/Apple Music Activity/Apple Music Play Activity.csv","Apple_Media_Services/Apple Music Activity/Apple Music - Play History Daily Tracks.csv","other.csv"]}`)
	if code != 200 || out["format"] != "applemusic" || len(out["roles"].([]any)) != 2 {
		t.Fatalf("apple zip: %d %v", code, out)
	}
	if code, out := check(`{"entries":["readme.txt"]}`); code != 422 {
		t.Fatalf("unknown zip: %d %v", code, out)
	}
}
