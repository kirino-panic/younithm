package providers

import "testing"

func TestParseURLKinds(t *testing.T) {
	cases := []struct{ kind, in, p, id string }{
		{"release", "https://open.spotify.com/album/6LePulMIQfALTo3KcCVqYC", "spotify", "6LePulMIQfALTo3KcCVqYC"},
		{"release", "https://music.apple.com/us/album/nocturnal-single/1749310351", "apple", "1749310351"},
		{"release", "https://music.apple.com/us/album/nocturnal-single/1749310351?i=1749310587", "", ""},
		{"release", "https://musicbrainz.org/release/0b4b5f6c-1234-4abc-9def-0123456789ab", "musicbrainz", "0b4b5f6c-1234-4abc-9def-0123456789ab"},
		{"artist", "https://open.spotify.com/artist/2UwJRAgSOi1zcLkvUNc8XL", "spotify", "2UwJRAgSOi1zcLkvUNc8XL"},
		{"artist", "https://music.apple.com/us/artist/reol/1163095812", "apple", "1163095812"},
		{"artist", "https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC", "", ""},
		{"recording", "https://music.apple.com/us/album/thriller/269572838?i=269573364", "apple", "269573364"},
	}
	for _, c := range cases {
		p, id, ok := ParseURL(c.kind, c.in)
		if ok != (c.p != "") || p != c.p || id != c.id {
			t.Errorf("%s %s: got %q %q %v", c.kind, c.in, p, id, ok)
		}
	}
}
