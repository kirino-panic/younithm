package musicbrainz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"younithm/internal/providers"
)

func TestDynamicFollowsContact(t *testing.T) {
	var contact atomic.Value // string
	contact.Store("")
	var gotUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA.Store(r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(`{"recordings":[]}`))
	}))
	defer srv.Close()
	d := NewDynamic(func(context.Context) (string, error) {
		c := contact.Load().(string)
		if c == "" {
			return "", providers.ErrNotConfigured
		}
		return c, nil
	}, Config{BaseURL: srv.URL})

	ctx := context.Background()
	if d.Configured(ctx) {
		t.Fatal("configured without a contact")
	}
	if _, err := d.SearchTrack(ctx, providers.TrackQuery{Artist: "a", Title: "b"}); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := d.LookupISRC(ctx, "USRC17607839"); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("ISRC lookup without contact: %v", err)
	}

	contact.Store("first@example.invalid")
	if _, err := d.SearchTrack(ctx, providers.TrackQuery{Artist: "a", Title: "b"}); err != nil {
		t.Fatal(err)
	}
	if ua, _ := gotUA.Load().(string); !strings.Contains(ua, "first@example.invalid") {
		t.Fatalf("User-Agent %q lacks the contact", ua)
	}
	contact.Store("second@example.invalid")
	if _, err := d.SearchTrack(ctx, providers.TrackQuery{Artist: "a", Title: "b"}); err != nil {
		t.Fatal(err)
	}
	if ua, _ := gotUA.Load().(string); !strings.Contains(ua, "second@example.invalid") {
		t.Fatalf("contact change not picked up: %q", ua)
	}
	if d.UserAgent(ctx) == "" || d.Name() != providers.MusicBrainz {
		t.Fatal("name or user agent")
	}
}
