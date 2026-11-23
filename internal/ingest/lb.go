package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ErrInvalidToken is returned by an Authenticator for unknown tokens.
var ErrInvalidToken = errors.New("invalid token")

// Authenticator validates a ListenBrainz-style token for the submit scope.
type Authenticator interface {
	AuthenticateToken(ctx context.Context, token string) (userID int64, userName string, err error)
}

const (
	maxBody       = 10 << 20
	maxImport     = 1000
	minListenedAt = 1033430400 // 2002-09-30, as ListenBrainz
)

type lbRequest struct {
	ListenType string            `json:"listen_type"`
	Payload    []json.RawMessage `json:"payload"`
}

type lbListen struct {
	ListenedAt    *int64 `json:"listened_at"`
	TrackMetadata struct {
		ArtistName     string         `json:"artist_name"`
		TrackName      string         `json:"track_name"`
		ReleaseName    string         `json:"release_name"`
		AdditionalInfo map[string]any `json:"additional_info"`
	} `json:"track_metadata"`
}

func lbError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"code": code, "error": msg})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	for _, p := range []string{"Token ", "Bearer "} {
		if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
			return strings.TrimSpace(h[len(p):]), true
		}
	}
	return "", false
}

// The token may be missing: the Authenticator decides (the admin can choose
// to accept scrobbles without one).
func (h *Handler) validateToken(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearer(r)
	_, name, err := h.auth.AuthenticateToken(r.Context(), tok)
	if err != nil && !ok {
		lbError(w, 401, "You need to provide an Authorization header.")
		return
	}
	if err != nil {
		writeJSON(w, 200, map[string]any{"code": 200, "message": "Token invalid.", "valid": false})
		return
	}
	writeJSON(w, 200, map[string]any{"code": 200, "message": "Token valid.", "valid": true, "user_name": name})
}

func (h *Handler) submitListens(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearer(r)
	uid, _, err := h.auth.AuthenticateToken(r.Context(), tok)
	if err != nil && !ok {
		lbError(w, 401, "You need to provide an Authorization header.")
		return
	}
	if err != nil {
		lbError(w, 401, "Invalid authorization token.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		lbError(w, 400, "Could not read request body (too large?).")
		return
	}
	var req lbRequest
	if err := json.Unmarshal(body, &req); err != nil {
		lbError(w, 400, "Invalid JSON: "+err.Error())
		return
	}
	n := len(req.Payload)
	switch req.ListenType {
	case "single", "playing_now":
		if n != 1 {
			lbError(w, 400, fmt.Sprintf("JSON document must contain exactly one listen for %q.", req.ListenType))
			return
		}
	case "import":
		if n == 0 || n > maxImport {
			lbError(w, 400, fmt.Sprintf("Import payload must contain 1..%d listens.", maxImport))
			return
		}
	default:
		lbError(w, 400, "Invalid listen_type.")
		return
	}
	now := time.Now()
	var batch []NewScrobble
	var npTrack NowPlayingTrack
	for i, raw := range req.Payload {
		var l lbListen
		if err := json.Unmarshal(raw, &l); err != nil {
			lbError(w, 400, fmt.Sprintf("Listen %d: invalid structure.", i))
			return
		}
		artist, title := strings.TrimSpace(l.TrackMetadata.ArtistName), strings.TrimSpace(l.TrackMetadata.TrackName)
		if artist == "" || title == "" {
			lbError(w, 400, fmt.Sprintf("Listen %d: artist_name and track_name are required.", i))
			return
		}
		if len(artist) > 1000 || len(title) > 1000 || len(l.TrackMetadata.ReleaseName) > 1000 {
			lbError(w, 400, fmt.Sprintf("Listen %d: metadata field too long.", i))
			return
		}
		ai := l.TrackMetadata.AdditionalInfo
		durMs := int64(numField(ai, "duration_ms"))
		if durMs == 0 {
			durMs = int64(numField(ai, "duration") * 1000)
		}
		album := strings.TrimSpace(l.TrackMetadata.ReleaseName)
		if req.ListenType == "playing_now" {
			if l.ListenedAt != nil {
				lbError(w, 400, "playing_now must not include listened_at.")
				return
			}
			npTrack = NowPlayingTrack{Artist: artist, Title: title, Album: album, DurationMs: durMs,
				RecordingMBID: strField(ai, "recording_mbid"), ReleaseMBID: strField(ai, "release_mbid")}
			h.setNowPlaying(r.Context(), uid, npTrack, clientName(ai), raw)
			writeJSON(w, 200, map[string]any{"status": "ok", "accepted": 0, "duplicates": 0})
			return
		}
		if l.ListenedAt == nil {
			lbError(w, 400, fmt.Sprintf("Listen %d: listened_at is required.", i))
			return
		}
		ts := *l.ListenedAt
		if ts < minListenedAt || ts > now.Add(24*time.Hour).Unix() {
			lbError(w, 400, fmt.Sprintf("Listen %d: listened_at out of range.", i))
			return
		}
		batch = append(batch, NewScrobble{
			UserID: uid, PlayedAt: time.Unix(ts, 0).UTC(), TrackDurationMs: durMs,
			Artist: artist, Title: title, Album: album,
			RecordingMBID: strField(ai, "recording_mbid"), ReleaseMBID: strField(ai, "release_mbid"),
			ArtistMBIDs: strList(ai, "artist_mbids"), ISRC: strField(ai, "isrc"),
			Source: clientName(ai), RawPayload: raw,
		})
	}
	rels := make([]int64, len(batch))
	acc, dup, err := h.store.insert(r.Context(), batch, rels)
	if err != nil {
		log.Printf("ingest: insert: %v", err)
		lbError(w, 500, "Internal error storing listens.")
		return
	}
	if req.ListenType == "single" {
		h.keepCover(rels[0], h.np.ClearIfMatches(uid, batch[0].Artist, batch[0].Title))
	}
	if acc > 0 {
		h.np.Scrobbled()
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "accepted": acc, "duplicates": dup})
}

func numField(m map[string]any, k string) float64 {
	if f, ok := m[k].(float64); ok && f > 0 {
		return f
	}
	return 0
}

func strField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func strList(m map[string]any, k string) []string {
	var out []string
	if a, ok := m[k].([]any); ok {
		for _, v := range a {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func clientName(ai map[string]any) string {
	if c := strField(ai, "submission_client"); c != "" {
		if v := strField(ai, "submission_client_version"); v != "" {
			return c + " " + v
		}
		return c
	}
	if p := strField(ai, "media_player"); p != "" {
		return p
	}
	return "listenbrainz"
}
