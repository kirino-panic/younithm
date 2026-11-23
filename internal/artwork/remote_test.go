package artwork

import (
	"context"
	"errors"
	"testing"

	"younithm/internal/providers"
)

// withTracks is a fake that also knows which album a track is on.
type withTracks struct {
	*fake
	albumOf map[string]string
}

func (f *withTracks) GetTrack(_ context.Context, id string) (*providers.Track, error) {
	if a, ok := f.albumOf[id]; ok {
		return &providers.Track{ProviderID: id, Releases: []providers.ReleaseRef{{ProviderID: a, Title: "Album"}}}, nil
	}
	return nil, providers.ErrNotFound
}

func registry(ps ...providers.Provider) *providers.Registry {
	reg := providers.NewRegistry()
	for _, p := range ps {
		reg.Add(p)
	}
	return reg
}

func TestRemoteTriesAlbumIDsThenTrackIDs(t *testing.T) {
	sp := &withTracks{&fake{name: providers.Spotify, art: map[string][]providers.ImageRef{"alb": {{URL: "sp", Kind: "front"}}}}, map[string]string{"trk": "alb"}}
	mb := &fake{name: providers.MusicBrainz, art: map[string][]providers.ImageRef{"rel": {{URL: "mb", Kind: "front"}}}}
	r := &Remote{Registry: registry(sp, mb)}
	ctx := context.Background()

	if f := r.Find(ctx, RemoteIDs{SpotifyAlbum: "missing", ReleaseMBID: "rel"}); f != (Found{URL: "mb"}) {
		t.Fatalf("album ids: %+v", f)
	}
	if f := r.Find(ctx, RemoteIDs{SpotifyTrack: "trk"}); f != (Found{URL: "sp", Album: "Album"}) {
		t.Fatalf("track id: %+v", f)
	}
	if f := r.Find(ctx, RemoteIDs{}); f.URL != "" {
		t.Fatalf("no ids: %+v", f)
	}
	// Remembered: asking again makes no request.
	n := mb.release.Load()
	r.Find(ctx, RemoteIDs{SpotifyAlbum: "missing", ReleaseMBID: "rel"})
	if mb.release.Load() != n {
		t.Fatal("asked MusicBrainz again")
	}
}

func TestRemoteDoesNotRememberOutages(t *testing.T) {
	mb := &fake{name: providers.MusicBrainz, err: errors.New("HTTP 502")}
	r := &Remote{Registry: registry(mb)}
	ids := RemoteIDs{ReleaseMBID: "rel"}
	r.Find(context.Background(), ids)
	r.Find(context.Background(), ids)
	if n := mb.release.Load(); n != 2 {
		t.Fatalf("asked %d times, want 2", n)
	}
}
