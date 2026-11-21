package imports

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"younithm/internal/importers"
	"younithm/internal/ingest"
)

// Candidate is one parsed listen prepared for dedupe and storage.
type Candidate struct {
	L        importers.Listen
	PlayedAt time.Time // UTC, truncated to the second like ingest stores it
	NArtist  string    // normalized for dedupe (ingest.Normalize)
	NTitle   string
	Key      uint64 // hash of (second, artist, title): exact in-file duplicate key
}

// MapListen is the listen-to-scrobble mapper. Raw artist/title/album stay as
// found in the file; only the stored payload is
// scrubbed (see SanitizePayload).
func MapListen(l importers.Listen) Candidate {
	c := Candidate{L: l, PlayedAt: l.PlayedAt.UTC().Truncate(time.Second)}
	c.NArtist, c.NTitle = ingest.Normalize(l.Artist), ingest.Normalize(l.Title)
	h := fnv.New64a()
	h.Write([]byte(strconv.FormatInt(c.PlayedAt.Unix(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(c.NArtist))
	h.Write([]byte{0})
	h.Write([]byte(c.NTitle))
	c.Key = h.Sum64()
	return c
}

// ToScrobble converts to the ingest write model. Source is "import:<format>".
func (c Candidate) ToScrobble() ingest.NewScrobble {
	l := c.L
	return ingest.NewScrobble{
		PlayedAt:         c.PlayedAt,
		DurationPlayedMs: l.DurationPlayedMs,
		TrackDurationMs:  l.TrackDurationMs,
		Artist:           l.Artist,
		Title:            l.Title,
		Album:            l.Album,
		RecordingMBID:    l.RecordingMBID,
		ReleaseMBID:      l.ReleaseMBID,
		ArtistMBIDs:      l.ArtistMBIDs,
		ISRC:             l.ISRC,
		Source:           "import:" + l.Source,
		RawPayload:       SanitizePayload(l.RawPayload),
	}
}

// piiKey reports whether a payload key holds personal data that must not be
// persisted: IP addresses (Spotify ip_addr, ip_addr_decrypted), the account's
// own name, coarse location, and contact details. Matching is
// case-insensitive on the key name, at any depth.
func piiKey(k string) bool {
	k = strings.ToLower(k)
	switch {
	case strings.HasPrefix(k, "ip_addr"), k == "ip", k == "ip_address", k == "ipaddress":
		return true
	case k == "user_name", k == "username", k == "user", k == "email", k == "user_agent":
		return true
	case k == "conn_country": // Spotify: country the stream was played from
		return true
	}
	return false
}

// SanitizePayload removes PII keys (see piiKey) from a record's JSON before
// it is stored in scrobble.raw_payload. Numbers keep their exact text.
// Unparseable input yields nil: better no payload than an unscrubbed one.
func SanitizePayload(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	out, err := json.Marshal(scrub(v))
	if err != nil {
		return nil
	}
	return out
}

func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if piiKey(k) {
				delete(t, k)
				continue
			}
			t[k] = scrub(x)
		}
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
	}
	return v
}
