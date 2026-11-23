package artwork

import (
	"context"
	"errors"
	"strings"
	"sync"

	"younithm/internal/providers"
)

// RemoteIDs are the catalogue ids a listen names, for finding a cover of a
// track that is not in the library (now playing). Album ids are tried before
// track ids, which cost one more request.
type RemoteIDs struct {
	SpotifyAlbum, AppleAlbum, ReleaseMBID string
	SpotifyTrack, AppleSong               string
}

func (ids RemoteIDs) key() string {
	return strings.Join([]string{ids.SpotifyAlbum, ids.AppleAlbum, ids.ReleaseMBID, ids.SpotifyTrack, ids.AppleSong}, "|")
}

// remoteCacheSize bounds the remembered answers; the cache starts over when full.
const remoteCacheSize = 500

// Found is a cover a provider has. Album is the provider's album title when
// it was found through a track id (the track's album there, which may be
// another edition); "" when it came from an album id.
type Found struct {
	URL, Album string
}

// Remote finds a cover URL on the providers by id, without downloading or
// storing it. Answers are remembered, so a client resending "playing now"
// does not ask again.
type Remote struct {
	Registry *providers.Registry

	mu    sync.Mutex
	cache map[string]Found
}

// Find returns the provider's cover for ids; its URL is "" when none has one.
func (r *Remote) Find(ctx context.Context, ids RemoteIDs) Found {
	k := ids.key()
	if k == "||||" {
		return Found{}
	}
	r.mu.Lock()
	u, ok := r.cache[k]
	r.mu.Unlock()
	if ok {
		return u
	}
	u, final := r.find(ctx, ids)
	if final { // a rate limit or outage is not an answer: ask again next time
		r.mu.Lock()
		if r.cache == nil || len(r.cache) >= remoteCacheSize {
			r.cache = map[string]Found{}
		}
		r.cache[k] = u
		r.mu.Unlock()
	}
	return u
}

var (
	spotifyOnly = []string{providers.Spotify}
	appleAny    = []string{providers.AppleMusic, providers.ITunes} // iTunes needs no key
	mbOnly      = []string{providers.MusicBrainz}
)

func (r *Remote) find(ctx context.Context, ids RemoteIDs) (Found, bool) {
	final := true
	try := func(names []string, album, track string) Found {
		for _, name := range names {
			p, ok := r.Registry.Get(name)
			if !ok {
				continue
			}
			id, title := album, ""
			if id == "" && track != "" {
				t, err := p.GetTrack(ctx, track)
				if !answered(err, &final) || t == nil || len(t.Releases) == 0 {
					continue
				}
				id, title = t.Releases[0].ProviderID, t.Releases[0].Title
			}
			if id == "" {
				continue
			}
			refs, err := p.GetArtwork(ctx, id)
			if !answered(err, &final) {
				continue
			}
			if ref, ok := pick(refs); ok {
				return Found{ref.URL, title}
			}
		}
		return Found{}
	}
	for _, step := range []struct {
		names        []string
		album, track string
	}{
		{spotifyOnly, ids.SpotifyAlbum, ""},
		{appleAny, ids.AppleAlbum, ""},
		{mbOnly, ids.ReleaseMBID, ""},
		{spotifyOnly, "", ids.SpotifyTrack},
		{appleAny, "", ids.AppleSong},
	} {
		if step.album == "" && step.track == "" {
			continue
		}
		if f := try(step.names, step.album, step.track); f.URL != "" {
			return f, true
		}
		if ctx.Err() != nil {
			return Found{}, false
		}
	}
	return Found{}, final
}

// answered reports whether err is a usable answer; a provider that is down or
// rate limited clears final.
func answered(err error, final *bool) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, providers.ErrNotFound), errors.Is(err, providers.ErrUnsupported), errors.Is(err, providers.ErrNotConfigured):
		return false
	default:
		*final = false
		return false
	}
}
