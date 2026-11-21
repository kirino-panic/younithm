package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexBaseHref(t *testing.T) {
	for base, want := range map[string]string{"": `<base href="/" />`, "/younithm": `<base href="/younithm/" />`} {
		rec := httptest.NewRecorder()
		Handler(base).ServeHTTP(rec, httptest.NewRequest("GET", "/some/route", nil))
		body := rec.Body.String()
		if strings.Contains(body, "frontend not built") {
			t.Skip("web/dist not built")
		}
		if !strings.Contains(body, want) {
			t.Errorf("base %q: index lacks %s", base, want)
		}
		if i, j := strings.Index(body, "<base"), strings.Index(body, "./assets/"); j >= 0 && i > j {
			t.Errorf("base %q: <base> comes after the asset links", base)
		}
	}
}
