package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func (e *adminEnv) upload(name string, data []byte) (int, string) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/admin/imports", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if e.cookie != nil {
		req.AddCookie(e.cookie)
		req.Header.Set("X-CSRF-Token", e.csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestImportEndpointsAreAdminOnly(t *testing.T) {
	e := newAdminEnv(t)
	data, err := os.ReadFile(filepath.Join("..", "importers", "testdata", "listenbrainz.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const p = "/api/v1/admin/imports"

	if code, _ := e.upload("lb.jsonl", data); code != 401 {
		t.Fatalf("upload without session: %d", code)
	}
	for _, m := range []string{"GET", "POST"} {
		path := p
		if m == "POST" {
			path += "/1/confirm"
		}
		if resp, _ := e.do(m, path, nil); resp.StatusCode != 401 {
			t.Fatalf("%s without session: %d", m, resp.StatusCode)
		}
	}

	e.setup()
	code, body := e.upload("lb.jsonl", data)
	if code != 201 {
		t.Fatalf("upload: %d %s", code, body)
	}
	var job struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &job)

	var status string
	for i := 0; i < 500 && status != "awaiting_confirmation"; i++ {
		_, b := e.do("GET", p+"/"+itoa(job.ID), nil)
		var j struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal([]byte(b), &j)
		status = j.Status
		time.Sleep(10 * time.Millisecond)
	}
	if status != "awaiting_confirmation" {
		t.Fatalf("job never reached preview: %s", status)
	}

	// Confirming needs only the session, no password.
	if resp, b := e.do("POST", p+"/"+itoa(job.ID)+"/confirm", nil); resp.StatusCode != 200 {
		t.Fatalf("confirm: %d %s", resp.StatusCode, b)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
