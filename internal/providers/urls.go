package providers

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	mbidRe      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	spotifyIDRe = regexp.MustCompile(`^[0-9A-Za-z]{22}$`)
	appleIDRe   = regexp.MustCompile(`^[0-9]{1,15}$`)
)

// ParseURL reads a catalogue link a person pasted for an entity kind
// ("recording", "release" or "artist"): a MusicBrainz URL or bare MBID, a
// Spotify URL or URI, or an Apple Music URL (a song may be an album URL with
// ?i=). provider is "" when the link is not one of those for that kind.
func ParseURL(kind, raw string) (provider, id string, ok bool) {
	s := strings.TrimSpace(raw)
	mbPath := map[string]string{"recording": "recording", "release": "release", "artist": "artist"}[kind]
	spPath := map[string]string{"recording": "track", "release": "album", "artist": "artist"}[kind]
	apPath := map[string]string{"recording": "song", "release": "album", "artist": "artist"}[kind]
	if mbPath == "" {
		return "", "", false
	}
	if mbidRe.MatchString(strings.ToLower(s)) {
		return MusicBrainz, strings.ToLower(s), true
	}
	if rest, found := strings.CutPrefix(s, "spotify:"+spPath+":"); found && spotifyIDRe.MatchString(rest) {
		return Spotify, rest, true
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch host {
	case "musicbrainz.org", "beta.musicbrainz.org":
		if len(parts) >= 2 && parts[0] == mbPath && mbidRe.MatchString(strings.ToLower(parts[1])) {
			return MusicBrainz, strings.ToLower(parts[1]), true
		}
	case "open.spotify.com":
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == spPath && spotifyIDRe.MatchString(parts[i+1]) {
				return Spotify, parts[i+1], true
			}
		}
	case "music.apple.com", "itunes.apple.com":
		if kind == "recording" {
			if i := u.Query().Get("i"); appleIDRe.MatchString(i) {
				return AppleMusic, i, true
			}
		}
		last := parts[len(parts)-1]
		for _, p := range parts {
			if p == apPath && appleIDRe.MatchString(last) && !(kind == "release" && u.Query().Get("i") != "") {
				return AppleMusic, last, true
			}
		}
	}
	return "", "", false
}
