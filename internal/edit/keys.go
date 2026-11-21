package edit

import (
	"strings"
	"unicode"
)

// Norm is the alias-key normalizer. It must equal ingest.Normalize (a test
// asserts this): lowercase, trim, collapse whitespace.
func Norm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// Alias kinds, equal to alias.kind.
const (
	KindArtist    = "artist"
	KindRecording = "recording"
	KindRelease   = "release"
)

const keySep = "\x1f"

// ArtistKey is the alias raw_string for an artist name.
func ArtistKey(name string) string { return Norm(name) }

// RecordingKey is the alias raw_string for a recording as submitted: the
// normalized artist string and title joined by a unit separator. (Titles alone
// are ambiguous; "Intro" exists on many artists.)
func RecordingKey(artist, title string) string { return Norm(artist) + keySep + Norm(title) }

// ReleaseKey is the alias raw_string for an album title.
func ReleaseKey(title string) string { return Norm(title) }
