// Package itunes is the keyless iTunes Search API provider: the fallback for
// Apple catalogue data when no Apple Music developer key is configured.
//
// It has no ISRC lookup (the API does not expose ISRCs), and Apple documents
// only a rough limit (about 20 calls a minute), so the client spaces calls.
// Endpoint and field names are from the public docs as remembered and have not
// been run against the live service from this repository.
package itunes

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
	"time"

	"younithm/internal/providers"
)

const (
	DefaultBaseURL = "https://itunes.apple.com"
	maxBody        = 4 << 20
)

type Config struct {
	BaseURL string
	Country string // storefront, default "us"
	// Interval between requests; default 3s (about 20 a minute).
	Interval   time.Duration
	HTTPClient *http.Client
	Limiter    *providers.Limiter
}

type Client struct {
	cfg Config
	hc  *http.Client
	lim *providers.Limiter
}

var _ providers.Provider = (*Client)(nil)

func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Country == "" {
		cfg.Country = "us"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 3 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	lim := cfg.Limiter
	if lim == nil {
		lim = providers.NewLimiter(cfg.Interval)
	}
	return &Client{cfg: cfg, hc: hc, lim: lim}
}

func (c *Client) Name() string { return providers.ITunes }

type result struct {
	WrapperType    string `json:"wrapperType"`
	Kind           string `json:"kind"`
	TrackID        int64  `json:"trackId"`
	TrackName      string `json:"trackName"`
	ArtistID       int64  `json:"artistId"`
	ArtistName     string `json:"artistName"`
	CollectionID   int64  `json:"collectionId"`
	CollectionName string `json:"collectionName"`
	TrackTimeMs    int    `json:"trackTimeMillis"`
	ReleaseDate    string `json:"releaseDate"`
	DiscNumber     int    `json:"discNumber"`
	TrackNumber    int    `json:"trackNumber"`
	Artwork100     string `json:"artworkUrl100"`
}

func (r result) toTrack() providers.Track {
	t := providers.Track{
		Provider: providers.ITunes, ProviderID: strconv.FormatInt(r.TrackID, 10),
		Title: r.TrackName, DurationMs: r.TrackTimeMs,
		Artists: []providers.Artist{{ProviderID: strconv.FormatInt(r.ArtistID, 10), Name: r.ArtistName}},
	}
	if r.CollectionName != "" {
		date := r.ReleaseDate
		if len(date) >= 10 {
			date = date[:10] // "2001-02-03T08:00:00Z" -> date
		}
		t.Releases = []providers.ReleaseRef{{
			ProviderID: strconv.FormatInt(r.CollectionID, 10), Title: r.CollectionName,
			Date: date, DateIsTrack: date != "", Disc: r.DiscNumber, Position: r.TrackNumber,
		}}
	}
	return t
}

type response struct {
	Results []result `json:"results"`
}

// SearchTrack searches songs by "artist title". The API has no field filters,
// so the matcher does the real scoring on the results.
func (c *Client) SearchTrack(ctx context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	term := strings.Join(strings.Fields(q.Artist+" "+q.Title), " ")
	if term == "" {
		return nil, errors.New("itunes: empty query")
	}
	limit := q.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	v := url.Values{"term": {term}, "media": {"music"}, "entity": {"song"},
		"limit": {strconv.Itoa(limit)}, "country": {c.cfg.Country}}
	var out response
	if err := c.get(ctx, "/search?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	tracks := make([]providers.Track, 0, len(out.Results))
	for _, r := range out.Results {
		if r.TrackID != 0 && (r.Kind == "" || r.Kind == "song") {
			tracks = append(tracks, r.toTrack())
		}
	}
	return tracks, nil
}

func (c *Client) GetTrack(ctx context.Context, id string) (*providers.Track, error) {
	if !digits(id) {
		return nil, providers.ErrNotFound
	}
	var out response
	if err := c.get(ctx, "/lookup?id="+id+"&country="+c.cfg.Country, &out); err != nil {
		return nil, err
	}
	for _, r := range out.Results {
		if r.TrackID != 0 {
			t := r.toTrack()
			return &t, nil
		}
	}
	return nil, providers.ErrNotFound
}

// GetArtwork takes a collection (album) id. iTunes serves one image template;
// the size in the URL can be changed, so a few sizes are offered.
func (c *Client) GetArtwork(ctx context.Context, collectionID string) ([]providers.ImageRef, error) {
	if !digits(collectionID) {
		return nil, providers.ErrNotFound
	}
	var out response
	if err := c.get(ctx, "/lookup?id="+collectionID+"&country="+c.cfg.Country, &out); err != nil {
		return nil, err
	}
	for _, r := range out.Results {
		if r.Artwork100 == "" {
			continue
		}
		var imgs []providers.ImageRef
		for _, size := range []int{1000, 600, 100} {
			imgs = append(imgs, providers.ImageRef{
				URL: resize(r.Artwork100, size), Width: size, Height: size, Kind: "front",
			})
		}
		return imgs, nil
	}
	return nil, providers.ErrNotFound
}

// GetArtistImage: the iTunes API has no artist images.
func (c *Client) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}

// resize swaps the "100x100" size token in an artworkUrl100 for another size.
func resize(u string, size int) string {
	s := strconv.Itoa(size)
	return strings.Replace(u, "100x100", s+"x"+s, 1)
}

func digits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *Client) get(ctx context.Context, path string, dst any) error {
	if err := c.lim.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusServiceUnavailable:
		// iTunes answers 403 when throttled.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		ra := 60
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ra = s
		}
		c.lim.Penalize(time.Duration(ra) * time.Second)
		return &providers.RateLimitedError{Provider: providers.ITunes, RetryAfter: ra}
	case resp.StatusCode != http.StatusOK:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("itunes: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	// Served as text/javascript; the body is still plain JSON.
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(dst)
}
