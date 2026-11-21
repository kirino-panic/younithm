// Package identity resolves a listen to artist, recording and release rows
// following docs/identity.md: our own row is the identity,
// provider ids are external_link rows, and a duplicate is preferred over a
// wrong merge.
package identity

import (
	"encoding/json"
	"net/url"
	"strings"
	"unicode"
)

// Provider names, equal to external_link.provider.
const (
	MusicBrainz = "musicbrainz"
	Spotify     = "spotify"
	Apple       = "apple"
	ISRCProv    = "isrc"
	// UPCProv is a release's barcode (providers.NormalizeBarcode form): the key
	// that says two catalogues' albums are the same release.
	UPCProv = "upc"
)

// Credit is one credited artist, in credit order.
type Credit struct {
	Name       string
	MBID       string
	SpotifyID  string // bare id
	AppleID    string // bare numeric id (weak key)
	JoinPhrase string // text that follows this credit when displayed
}

// Input is everything one listen tells us about identity. Empty means unknown.
type Input struct {
	Artist, Title, Album string // as submitted
	DurationMs           int64

	Credits      []Credit
	AlbumArtists []Credit

	RecordingMBID, ReleaseMBID, ReleaseGroupMBID string
	SpotifyTrackID, SpotifyAlbumID               string
	AppleSongID, AppleAlbumID, ISRC              string

	// Tier C: ListenBrainz mbid_mapping ids that were NOT corroborated. They
	// are stored as candidates only.
	CandRecordingMBID string
	CandArtistMBIDs   []string // aligned with Credits, "" where none

	// HintRecordingID/HintReleaseID: the scrobble already points here (backfill
	// keeps and attaches). Zero when unknown.
	HintRecordingID, HintReleaseID int64
}

// Norm lowercases, trims and collapses whitespace.
func Norm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// Parse overlays what a ListenBrainz-shaped payload (a listen with
// track_metadata, as submitted or exported) carries onto base. base holds the
// raw strings and any ids the caller already extracted. A payload that is not
// ListenBrainz-shaped leaves base's single credit untouched.
func Parse(raw json.RawMessage, base Input) Input {
	in := base
	var p struct {
		TM struct {
			ArtistName  string         `json:"artist_name"`
			TrackName   string         `json:"track_name"`
			ReleaseName string         `json:"release_name"`
			AI          map[string]any `json:"additional_info"`
			Mapping     *struct {
				RecordingName string `json:"recording_name"`
				RecordingMBID string `json:"recording_mbid"`
				ReleaseMBID   string `json:"release_mbid"`
				RGMBID        string `json:"release_group_mbid"`
				Artists       []struct {
					Name       string `json:"artist_credit_name"`
					MBID       string `json:"artist_mbid"`
					JoinPhrase string `json:"join_phrase"`
				} `json:"artists"`
				URLRels []struct {
					URL string `json:"url"`
				} `json:"url_rels"`
			} `json:"mbid_mapping"`
		} `json:"track_metadata"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	ai := p.TM.AI
	if ai == nil {
		ai = map[string]any{}
	}
	if in.RecordingMBID == "" {
		in.RecordingMBID = str(ai, "recording_mbid")
	}
	if in.ReleaseMBID == "" {
		in.ReleaseMBID = str(ai, "release_mbid")
	}
	if in.ReleaseGroupMBID == "" {
		in.ReleaseGroupMBID = str(ai, "release_group_mbid")
	}
	if in.ISRC == "" {
		in.ISRC = strings.ToUpper(str(ai, "isrc"))
	}
	if in.SpotifyTrackID == "" {
		in.SpotifyTrackID = SpotifyID(str(ai, "spotify_id"))
	}
	if in.SpotifyAlbumID == "" {
		in.SpotifyAlbumID = SpotifyID(str(ai, "spotify_album_id"))
	}
	if origin := str(ai, "origin_url"); strings.Contains(origin, "music.apple.com") {
		if in.AppleSongID == "" {
			in.AppleSongID = AppleSongFromURL(origin)
		}
		if in.AppleAlbumID == "" {
			in.AppleAlbumID = AppleAlbumFromURL(origin)
		}
	}
	if in.DurationMs == 0 {
		if v := num(ai, "duration_ms"); v > 0 {
			in.DurationMs = int64(v)
		} else if v := num(ai, "duration"); v > 0 {
			in.DurationMs = int64(v * 1000)
		}
	}

	names := strList(ai, "artist_names")
	var mapped []Credit
	var m = p.TM.Mapping
	if m != nil {
		for _, a := range m.Artists {
			mapped = append(mapped, Credit{Name: a.Name, MBID: a.MBID, JoinPhrase: a.JoinPhrase})
		}
	}
	switch {
	case len(names) > 0:
		in.Credits = make([]Credit, len(names))
		for i, n := range names {
			in.Credits[i].Name = n
		}
	case len(mapped) > 0:
		in.Credits = make([]Credit, len(mapped))
		for i, c := range mapped {
			in.Credits[i] = Credit{Name: c.Name, JoinPhrase: c.JoinPhrase}
		}
	}
	// IDs pair with credits by position only when the lists have equal length.
	if len(names) > 0 {
		pair := func(ids []string, set func(*Credit, string)) {
			if len(ids) == len(in.Credits) {
				for i, id := range ids {
					set(&in.Credits[i], id)
				}
			}
		}
		pair(strList(ai, "artist_mbids"), func(c *Credit, v string) { c.MBID = v })
		sp := strList(ai, "spotify_artist_ids")
		for i := range sp {
			sp[i] = SpotifyID(sp[i])
		}
		pair(sp, func(c *Credit, v string) { c.SpotifyID = v })
	}
	// Join phrases come from the mapping only when its credits are the same
	// artists in the same order.
	if len(mapped) == len(in.Credits) && len(mapped) > 0 {
		same := true
		for i := range mapped {
			if Norm(mapped[i].Name) != Norm(in.Credits[i].Name) {
				same = false
			}
		}
		if same {
			for i := range mapped {
				in.Credits[i].JoinPhrase = mapped[i].JoinPhrase
			}
		}
	}
	if len(in.Credits) == 0 && Norm(in.Artist) != "" {
		in.Credits = []Credit{{Name: strings.TrimSpace(in.Artist)}}
	}
	// Album artists.
	if an := strList(ai, "release_artist_names"); len(an) > 0 {
		in.AlbumArtists = make([]Credit, len(an))
		for i, n := range an {
			in.AlbumArtists[i].Name = n
		}
		if ids := strList(ai, "spotify_album_artist_ids"); len(ids) == len(an) {
			for i, id := range ids {
				in.AlbumArtists[i].SpotifyID = SpotifyID(id)
			}
		}
	}

	// Tier C: mbid_mapping. Auto only when it agrees with a client-submitted
	// id (a Spotify/Apple id in url_rels equal to ours, or the same recording
	// MBID). Otherwise candidates. It never overrides a submitted id.
	if m != nil {
		corroborated := false
		if m.RecordingMBID != "" && m.RecordingMBID == in.RecordingMBID {
			corroborated = true
		}
		for _, rel := range m.URLRels {
			if id := SpotifyID(rel.URL); id != "" && strings.Contains(rel.URL, "spotify") && id == in.SpotifyTrackID {
				corroborated = true
			}
			if strings.Contains(rel.URL, "music.apple.com") || strings.Contains(rel.URL, "itunes.apple.com") {
				if id := AppleID(rel.URL); id != "" && id == in.AppleSongID {
					corroborated = true
				}
			}
		}
		namesAgree := len(mapped) == len(in.Credits) && len(mapped) > 0
		for i := 0; namesAgree && i < len(mapped); i++ {
			if Norm(mapped[i].Name) != Norm(in.Credits[i].Name) {
				namesAgree = false
			}
		}
		submitted := in.RecordingMBID != ""
		if corroborated {
			fill := func(dst *string, v string) {
				if *dst == "" {
					*dst = v
				}
			}
			fill(&in.RecordingMBID, m.RecordingMBID)
			fill(&in.ReleaseMBID, m.ReleaseMBID)
			fill(&in.ReleaseGroupMBID, m.RGMBID)
			if namesAgree {
				for i := range mapped {
					if in.Credits[i].MBID == "" {
						in.Credits[i].MBID = mapped[i].MBID
					}
				}
			}
		} else if !submitted && m.RecordingMBID != "" {
			in.CandRecordingMBID = m.RecordingMBID
			if namesAgree {
				in.CandArtistMBIDs = make([]string, len(mapped))
				for i := range mapped {
					if in.Credits[i].MBID == "" {
						in.CandArtistMBIDs[i] = mapped[i].MBID
					}
				}
			}
		}
	}
	return in
}

// SpotifyID extracts the bare id from an open.spotify.com URL or spotify: URI;
// a bare id is returned as is.
func SpotifyID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "spotify:") {
		parts := strings.Split(s, ":")
		return parts[len(parts)-1]
	}
	if strings.Contains(s, "/") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		return segs[len(segs)-1]
	}
	return s
}

// AppleID extracts the numeric catalog id from an Apple Music URL (storefront
// ignored): ".../song/<id>", ".../song/<name>/<id>", or "?i=<id>".
func AppleID(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	if i := u.Query().Get("i"); isDigits(i) {
		return i
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if last := segs[len(segs)-1]; isDigits(last) {
		return last
	}
	if isDigits(s) {
		return s
	}
	return ""
}

// AppleSongFromURL extracts the song id from an Apple Music URL: the "?i="
// parameter of an album URL, or the last segment of ".../song/<name>/<id>".
// An album URL without "?i=" names no song.
func AppleSongFromURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	if i := u.Query().Get("i"); isDigits(i) {
		return i
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, seg := range segs {
		if seg == "song" && isDigits(segs[len(segs)-1]) && i < len(segs)-1 {
			return segs[len(segs)-1]
		}
	}
	return ""
}

// AppleAlbumFromURL extracts the album id from ".../album/<name>/<id>" (with
// or without "?i=<song id>"). A song-only URL names no album.
func AppleAlbumFromURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, seg := range segs {
		if seg == "album" && i < len(segs)-1 && isDigits(segs[len(segs)-1]) {
			return segs[len(segs)-1]
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func num(m map[string]any, k string) float64 {
	f, _ := m[k].(float64)
	return f
}

func strList(m map[string]any, k string) []string {
	var out []string
	if a, ok := m[k].([]any); ok {
		for _, v := range a {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

// Basics are the identity-relevant fields a caller (live ingest, an importer)
// has already extracted from a listen.
type Basics struct {
	Artist, Title, Album             string
	DurationMs                       int64
	RecordingMBID, ReleaseMBID, ISRC string
	ReleaseGroupMBID, SpotifyTrackID string
	ArtistMBIDs                      []string
	HintRecordingID, HintReleaseID   int64
}

// FromBasics builds an Input from the caller's fields and, when the raw
// payload is ListenBrainz-shaped, everything it adds (credit lists, ids,
// join phrases, mbid_mapping).
func FromBasics(raw json.RawMessage, b Basics) Input {
	base := Input{
		Artist: b.Artist, Title: b.Title, Album: b.Album, DurationMs: b.DurationMs,
		RecordingMBID: b.RecordingMBID, ReleaseMBID: b.ReleaseMBID, ReleaseGroupMBID: b.ReleaseGroupMBID,
		ISRC: strings.ToUpper(b.ISRC), SpotifyTrackID: b.SpotifyTrackID,
		HintRecordingID: b.HintRecordingID, HintReleaseID: b.HintReleaseID,
	}
	in := Parse(raw, base)
	if len(b.ArtistMBIDs) == len(in.Credits) {
		for i, id := range b.ArtistMBIDs {
			if in.Credits[i].MBID == "" {
				in.Credits[i].MBID = id
			}
		}
	}
	return in
}
