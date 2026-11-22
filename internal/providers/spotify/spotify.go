// Package spotify is the Spotify Web API metadata provider, using the client
// credentials flow (app-level access: catalogue search and lookups, no user
// data). Credentials are supplied lazily through a callback because the admin
// enters them in the UI after the server has started.
//
// The endpoint shapes below are from the public Web API reference as
// remembered, not verified against the live API in this repository. Spotify
// has changed access rules for new apps before, so treat a live 403 as a
// finding to investigate, not a bug here.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"younithm/internal/providers"
)

const (
	DefaultAPIURL   = "https://api.spotify.com/v1"
	DefaultTokenURL = "https://accounts.spotify.com/api/token"
	// DefaultLanguage is enforced as Accept-Language unless configured.
	DefaultLanguage = "en"
	maxBody         = 4 << 20
)

// Credentials returns the app's client id and secret. It should return
// providers.ErrNotConfigured (or empty values) when they are not set.
type Credentials func(ctx context.Context) (id, secret string, err error)

type Config struct {
	Credentials Credentials // required
	APIURL      string
	TokenURL    string
	HTTPClient  *http.Client
	Now         func() time.Time
	// Language is the Accept-Language every request carries, so names come back
	// in one known language whatever the server's locale is. Default "en".
	Language string
}

type Client struct {
	cfg Config
	hc  *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	forID   string // client id + secret the cached token was issued for

	tracklists providers.TracklistCache
}

var (
	_ providers.Provider    = (*Client)(nil)
	_ providers.ISRCLookup  = (*Client)(nil)
	_ providers.AlbumLookup = (*Client)(nil)

	_ providers.ReleaseTracksLookup = (*Client)(nil)

	_ providers.LocalizedNames = (*Client)(nil)
)

func New(cfg Config) (*Client, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("spotify: credentials callback required: %w", providers.ErrNotConfigured)
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = DefaultTokenURL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Language == "" {
		cfg.Language = DefaultLanguage
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg, hc: hc}, nil
}

func (c *Client) Name() string { return providers.Spotify }

// ---- wire types ----

type image struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type album struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ReleaseDate string `json:"release_date"`
	AlbumType   string `json:"album_type"`
	TotalTracks int    `json:"total_tracks"`
	ExternalIDs struct {
		UPC string `json:"upc"`
	} `json:"external_ids"` // only on a full album (/albums/{id}), not on a track's album
	Images  []image `json:"images"`
	Artists []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"artists"`
}

type track struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DurationMs  int    `json:"duration_ms"`
	DiscNumber  int    `json:"disc_number"`
	TrackNumber int    `json:"track_number"`
	Popularity  int    `json:"popularity"`
	Artists     []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"artists"`
	Album       album `json:"album"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
}

func (t track) toTrack() providers.Track {
	out := providers.Track{
		Provider: providers.Spotify, ProviderID: t.ID, Title: t.Name, DurationMs: t.DurationMs,
	}
	for _, a := range t.Artists {
		out.Artists = append(out.Artists, providers.Artist{ProviderID: a.ID, Name: a.Name})
	}
	if t.Album.ID != "" || t.Album.Name != "" {
		out.Releases = []providers.ReleaseRef{{
			ProviderID: t.Album.ID, Title: t.Album.Name, Date: t.Album.ReleaseDate,
			Type: strings.ToLower(t.Album.AlbumType), Disc: t.DiscNumber, Position: t.TrackNumber,
		}}
		for _, a := range t.Album.Artists {
			out.Releases[0].Artists = append(out.Releases[0].Artists, providers.Artist{ProviderID: a.ID, Name: a.Name})
		}
	}
	if t.ExternalIDs.ISRC != "" {
		out.ISRCs = []string{strings.ToUpper(t.ExternalIDs.ISRC)}
	}
	return out
}

func images(imgs []image, kind string) []providers.ImageRef {
	out := make([]providers.ImageRef, 0, len(imgs))
	for _, i := range imgs { // Spotify lists the largest first
		out = append(out, providers.ImageRef{URL: i.URL, Width: i.Width, Height: i.Height, Kind: kind})
	}
	return out
}

// ---- Provider methods ----

func (c *Client) search(ctx context.Context, q string, limit int) ([]track, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	v := url.Values{"q": {q}, "type": {"track"}, "limit": {strconv.Itoa(limit)}}
	var out struct {
		Tracks struct {
			Items []track `json:"items"`
		} `json:"tracks"`
	}
	if err := c.get(ctx, "/search?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	return out.Tracks.Items, nil
}

// SearchTrack runs a field-filtered catalogue search. The album is left out of
// the query (album names differ between releases and would hurt recall); the
// matcher scores it from the results instead.
func (c *Client) SearchTrack(ctx context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	if q.ISRC != "" && q.Title == "" {
		return c.LookupISRC(ctx, q.ISRC)
	}
	query := BuildQuery(q)
	if query == "" {
		return nil, errors.New("spotify: empty query")
	}
	items, err := c.search(ctx, query, q.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]providers.Track, 0, len(items))
	for _, t := range items {
		out = append(out, t.toTrack())
	}
	return out, nil
}

func (c *Client) GetTrack(ctx context.Context, id string) (*providers.Track, error) {
	if !validID(id) {
		return nil, providers.ErrNotFound
	}
	var t track
	if err := c.get(ctx, "/tracks/"+id, &t); err != nil {
		return nil, err
	}
	out := t.toTrack()
	return &out, nil
}

// Per-request id limits of the batch endpoints.
var batchSizes = map[string]int{providers.KindArtist: 50, providers.KindAlbum: 20, providers.KindRecording: 50}

func (c *Client) BatchSize(kind string) int { return batchSizes[kind] }

// NamesIn returns the names of up to BatchSize(kind) ids asked for with
// Accept-Language lang. Spotify localizes some artist names (米津玄師 in
// Japanese, Kenshi Yonezu in English) and silently keeps its one wording for
// the rest, so the answer is never authoritative about its language.
func (c *Client) NamesIn(ctx context.Context, kind string, ids []string, lang string) (map[string]string, bool, error) {
	key := map[string]string{providers.KindArtist: "artists", providers.KindAlbum: "albums", providers.KindRecording: "tracks"}[kind]
	if key == "" {
		return nil, false, providers.ErrUnsupported
	}
	var clean []string
	for _, id := range ids {
		if validID(id) {
			clean = append(clean, id)
		}
	}
	if max := batchSizes[kind]; len(clean) > max {
		clean = clean[:max]
	}
	out := map[string]string{}
	if len(clean) == 0 {
		return out, false, nil
	}
	var res map[string][]*struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.getIn(ctx, "/"+key+"?ids="+strings.Join(clean, ","), lang, &res); err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return out, false, nil
		}
		return nil, false, err
	}
	for _, it := range res[key] {
		if it != nil && strings.TrimSpace(it.Name) != "" {
			out[it.ID] = strings.TrimSpace(it.Name)
		}
	}
	return out, false, nil
}

// LookupISRC returns at most one track. Spotify lists the same recording once
// per release (album, single, compilation), all sharing the ISRC, so the
// copies are collapsed to the most popular one instead of being reported to
// the matcher as an ambiguity.
func (c *Client) LookupISRC(ctx context.Context, isrc string) ([]providers.Track, error) {
	isrc = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(isrc), "-", ""))
	if len(isrc) != 12 {
		return nil, providers.ErrNotFound
	}
	items, err := c.search(ctx, "isrc:"+isrc, 20)
	if err != nil {
		return nil, err
	}
	var best *track
	for i := range items {
		if !strings.EqualFold(items[i].ExternalIDs.ISRC, isrc) {
			continue
		}
		if best == nil || items[i].Popularity > best.Popularity {
			best = &items[i]
		}
	}
	if best == nil {
		return nil, nil
	}
	t := best.toTrack()
	return []providers.Track{t}, nil
}

// GetAlbum returns an album by its Spotify id, with its barcode (UPC).
func (c *Client) GetAlbum(ctx context.Context, id string) (*providers.Album, error) {
	if !validID(id) {
		return nil, providers.ErrNotFound
	}
	var a album
	if err := c.get(ctx, "/albums/"+id, &a); err != nil {
		return nil, err
	}
	return &providers.Album{ProviderID: a.ID, Title: a.Name, Date: a.ReleaseDate, UPC: a.ExternalIDs.UPC, TotalTracks: a.TotalTracks}, nil
}

// albumTracksPage is how many tracks one request of an album's tracklist
// returns (Spotify's maximum), and maxAlbumPages caps the requests per album.
const (
	albumTracksPage = 50
	maxAlbumPages   = 10
)

// LookupReleaseTracks returns an album's tracklist by its Spotify id, each
// track carrying the album with its disc and track number. The album request
// holds the first page; longer albums take one more request per page. Answers
// are cached. Spotify's album tracks carry no ISRC.
func (c *Client) LookupReleaseTracks(ctx context.Context, id string) ([]providers.Track, error) {
	if !validID(id) {
		return nil, providers.ErrNotFound
	}
	if ts, err, ok := c.tracklists.Get(id); ok {
		return ts, err
	}
	var a struct {
		album
		Tracks struct {
			Items []track `json:"items"`
			Total int     `json:"total"`
		} `json:"tracks"`
	}
	if err := c.get(ctx, "/albums/"+id, &a); err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			c.tracklists.Put(id, nil, true)
		}
		return nil, err
	}
	items := a.Tracks.Items
	for page := 1; len(items) < a.Tracks.Total && page < maxAlbumPages; page++ {
		var more struct {
			Items []track `json:"items"`
		}
		if err := c.get(ctx, fmt.Sprintf("/albums/%s/tracks?offset=%d&limit=%d", id, len(items), albumTracksPage), &more); err != nil {
			return nil, err
		}
		if len(more.Items) == 0 {
			break
		}
		items = append(items, more.Items...)
	}
	out := make([]providers.Track, 0, len(items))
	for _, it := range items {
		if it.ID == "" {
			continue // a track no longer available
		}
		it.Album = a.album
		out = append(out, it.toTrack())
	}
	c.tracklists.Put(id, out, false)
	return out, nil
}

// GetArtwork takes a Spotify album id.
func (c *Client) GetArtwork(ctx context.Context, albumID string) ([]providers.ImageRef, error) {
	if !validID(albumID) {
		return nil, providers.ErrNotFound
	}
	var a album
	if err := c.get(ctx, "/albums/"+albumID, &a); err != nil {
		return nil, err
	}
	return images(a.Images, "front"), nil
}

// GetArtistImage takes a Spotify artist id.
func (c *Client) GetArtistImage(ctx context.Context, artistID string) ([]providers.ImageRef, error) {
	if !validID(artistID) {
		return nil, providers.ErrNotFound
	}
	var a struct {
		Images []image `json:"images"`
	}
	if err := c.get(ctx, "/artists/"+artistID, &a); err != nil {
		return nil, err
	}
	return images(a.Images, "artist"), nil
}

// ---- auth ----

// accessToken returns a cached token, fetching a new one when it is missing,
// near expiry, or was issued for different credentials.
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	id, secret, err := c.cfg.Credentials(ctx)
	if err != nil {
		if errors.Is(err, providers.ErrNotConfigured) {
			return "", err
		}
		return "", fmt.Errorf("spotify: reading credentials: %w", err)
	}
	if id == "" || secret == "" {
		return "", fmt.Errorf("spotify: client id/secret not set: %w", providers.ErrNotConfigured)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.forID == id+"\x00"+secret && c.cfg.Now().Before(c.expires.Add(-30*time.Second)) {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL,
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return "", rateLimited(resp)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		c.token = ""
		return "", fmt.Errorf("spotify: credentials rejected (HTTP %d): %w", resp.StatusCode, providers.ErrNotConfigured)
	case resp.StatusCode != http.StatusOK:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("spotify: token HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("spotify: bad token response: %v", err)
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	c.token, c.forID = tok.AccessToken, id+"\x00"+secret
	c.expires = c.cfg.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.token, nil
}

func rateLimited(resp *http.Response) error {
	ra := 5
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		ra = s
	}
	return &providers.RateLimitedError{Provider: providers.Spotify, RetryAfter: ra}
}

// ---- HTTP ----

func (c *Client) get(ctx context.Context, path string, dst any) error {
	return c.getIn(ctx, path, "", dst)
}

func (c *Client) getIn(ctx context.Context, path, lang string, dst any) error {
	if lang == "" {
		lang = c.cfg.Language
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Language", lang)
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		err = func() error {
			defer resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusNotFound:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				return providers.ErrNotFound
			case resp.StatusCode == http.StatusTooManyRequests:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				return rateLimited(resp)
			case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				return errRetryAuth
			case resp.StatusCode != http.StatusOK:
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				return fmt.Errorf("spotify: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			}
			return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(dst)
		}()
		if errors.Is(err, errRetryAuth) {
			continue // token expired early: refresh once and retry
		}
		return err
	}
}

var errRetryAuth = errors.New("retry with fresh token")

func validID(s string) bool {
	if len(s) != 22 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// BuildQuery builds the field-filtered search query. Quotes and backslashes
// inside values are dropped: Spotify's query syntax has no reliable escape.
func BuildQuery(q providers.TrackQuery) string {
	clean := func(s string) string {
		return strings.Join(strings.Fields(strings.NewReplacer(`"`, " ", `\`, " ").Replace(s)), " ")
	}
	var parts []string
	if t := clean(q.Title); t != "" {
		parts = append(parts, `track:"`+t+`"`)
	}
	if a := clean(q.Artist); a != "" {
		parts = append(parts, `artist:"`+a+`"`)
	}
	return strings.Join(parts, " ")
}
