package musicbrainz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func batchClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Contact: "test@example.org", BaseURL: srv.URL, Interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const mbA = "2d5bd5f2-d0fa-4dd8-ba72-c1fea94a0ae0"

func TestArtistNamesLeavesOutLegalNames(t *testing.T) {
	var query string
	c := batchClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		w.Write([]byte(`{"artists":[{"id":"` + mbA + `","aliases":[
		  {"name":"Nightcord at 25:00","locale":"en","primary":true,"type":"Artist name"},
		  {"name":"N25","locale":null,"type":"Search hint"},
		  {"name":"Legal Person","locale":"en","primary":true,"type":"Legal name"},
		  {"name":"25時、ナイトコードで。","locale":"ja","primary":true},
		  {"name":"x","locale":"zh_Hant","primary":null}]}]}`))
	})
	got, err := c.ArtistNames(context.Background(), []string{mbA, "not-an-id"})
	if err != nil {
		t.Fatal(err)
	}
	if query != "arid:"+mbA {
		t.Fatalf("query %q", query)
	}
	ns := got[mbA]
	if len(ns) != 4 || ns[0].Name != "Nightcord at 25:00" || !ns[0].Primary || ns[1].Lang != "" || !ns[1].Hint ||
		ns[3].Lang != "zh-Hant" || ns[3].Primary || ns[3].Hint {
		t.Fatalf("names %+v", ns)
	}
}
