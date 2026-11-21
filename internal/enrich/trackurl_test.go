package enrich

import "testing"

func TestParseTrackURL(t *testing.T) {
	cases := []struct{ in, p, id string }{
		{"https://musicbrainz.org/recording/0B4B5F6C-1234-4abc-9def-0123456789ab", "musicbrainz", "0b4b5f6c-1234-4abc-9def-0123456789ab"},
		{"0b4b5f6c-1234-4abc-9def-0123456789ab", "musicbrainz", "0b4b5f6c-1234-4abc-9def-0123456789ab"},
		{"https://open.spotify.com/intl-de/track/4uLU6hMCjMI75M1A2tKUQC?si=x", "spotify", "4uLU6hMCjMI75M1A2tKUQC"},
		{"spotify:track:4uLU6hMCjMI75M1A2tKUQC", "spotify", "4uLU6hMCjMI75M1A2tKUQC"},
		{"https://music.apple.com/us/album/thriller/269572838?i=269573364", "apple", "269573364"},
		{"https://music.apple.com/us/song/billie-jean/269573364", "apple", "269573364"},
		{"https://musicbrainz.org/release/0b4b5f6c-1234-4abc-9def-0123456789ab", "", ""},
		{"https://example.com/track/4uLU6hMCjMI75M1A2tKUQC", "", ""},
	}
	for _, c := range cases {
		p, id, ok := ParseTrackURL(c.in)
		if ok != (c.p != "") || p != c.p || id != c.id {
			t.Errorf("%s: got %q %q %v", c.in, p, id, ok)
		}
	}
}
