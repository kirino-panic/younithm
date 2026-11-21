package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConnectionCheck(t *testing.T) {
	e := newAdminEnv(t)
	if resp, _ := e.do("POST", "/api/v1/admin/connections/spotify/check", nil); resp.StatusCode < 400 {
		t.Fatalf("anonymous check allowed: %d", resp.StatusCode)
	}
	e.setup()
	if resp, _ := e.do("POST", "/api/v1/admin/connections/nope/check", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown provider: %d", resp.StatusCode)
	}
	// Nothing saved yet: the check reports that rather than calling out.
	resp, body := e.do("POST", "/api/v1/admin/connections/spotify/check", nil)
	var out struct {
		OK   bool   `json:"ok"`
		Kind string `json:"error_kind"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != 200 || out.OK || out.Kind != "not_configured" {
		t.Fatalf("spotify unset: %d %s", resp.StatusCode, body)
	}

	// Details given but incomplete: refused without calling out, nothing saved.
	resp, body = e.do("POST", "/api/v1/admin/connections/spotify/check", map[string]any{"values": map[string]string{"spotify.client_id": "x"}})
	out.OK, out.Kind = true, ""
	_ = json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != 200 || out.OK || out.Kind != "missing" {
		t.Fatalf("spotify partial: %d %s", resp.StatusCode, body)
	}
	_, list := e.do("GET", "/api/v1/admin/secrets", nil)
	if strings.Contains(list, `"set":true`) {
		t.Fatalf("a check stored something: %s", list)
	}
}

func TestOnboardingDoneFlag(t *testing.T) {
	e := newAdminEnv(t)
	e.setup()
	if _, b := e.do("GET", "/api/v1/admin/onboarding", nil); !strings.Contains(b, `"done":false`) {
		t.Fatalf("a fresh install starts unfinished: %s", b)
	}
	if resp, _ := e.do("POST", "/api/v1/admin/onboarding/done", nil); resp.StatusCode != 200 {
		t.Fatalf("finish: %d", resp.StatusCode)
	}
	if _, b := e.do("GET", "/api/v1/admin/onboarding", nil); !strings.Contains(b, `"done":true`) {
		t.Fatalf("after finishing: %s", b)
	}
}
