package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithBasePath(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path + "|" + r.RequestURI))
	})
	h := withBasePath("/younithm", echo)
	cases := map[string]string{
		"/younithm/":                      "/|/",
		"/younithm/api/v1/recent?limit=3": "/api/v1/recent|/api/v1/recent?limit=3",
		"/api/v1/recent":                  "/api/v1/recent|/api/v1/recent", // proxy stripped it
		"/healthz":                        "/healthz|/healthz",
		"/younithmx/a":                    "/younithmx/a|/younithmx/a",
	}
	for in, want := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", in, nil))
		if got := rec.Body.String(); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/younithm?x=1", nil))
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "/younithm/?x=1" {
		t.Errorf("bare prefix: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if withBasePath("", echo) == nil {
		t.Fatal("nil handler")
	}
}
