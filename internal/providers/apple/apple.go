// Package apple is the Apple Music API provider. Requests carry a developer
// token: an ES256 JWT signed with the .p8 key from the Apple Developer
// account (team id, key id, private key). Only the catalogue is used; there
// is no user token and no access to anyone's library.
//
// The JWT format and endpoint shapes are from Apple's public documentation as
// remembered; they have not been run against the live API from this
// repository. Credentials are read through a callback so keys entered in the
// admin UI apply without a restart.
package apple

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"younithm/internal/providers"
)

const (
	DefaultBaseURL = "https://api.music.apple.com/v1"
	maxBody        = 4 << 20

	tokenTTL     = 12 * time.Hour // Apple allows up to 6 months; short keeps leaks cheap
	tokenRefresh = time.Hour      // rebuild when less than this remains
)

// Credentials returns the Apple developer team id, key id and the private key
// (PEM text of the .p8 file). Return providers.ErrNotConfigured when unset.
type Credentials func(ctx context.Context) (teamID, keyID, privateKey string, err error)

type Config struct {
	Credentials Credentials // required
	BaseURL     string
	Storefront  string // default "us"
	HTTPClient  *http.Client
	Now         func() time.Time
}

type Client struct {
	cfg Config
	hc  *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	forCred string

	fronts   []storefront // storefronts and their languages (names.go)
	frontsAt time.Time
}

var (
	_ providers.Provider    = (*Client)(nil)
	_ providers.ISRCLookup  = (*Client)(nil)
	_ providers.AlbumLookup = (*Client)(nil)
)

func New(cfg Config) (*Client, error) {
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("apple: credentials callback required: %w", providers.ErrNotConfigured)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Storefront == "" {
		cfg.Storefront = "us"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg, hc: hc}, nil
}

func (c *Client) Name() string { return providers.AppleMusic }

// ---- developer token ----

// ParsePrivateKey accepts the .p8 contents as pasted into a single field: PEM
// with real newlines, PEM with literal "\n" sequences, or the bare base64 body.
func ParsePrivateKey(s string) (*ecdsa.PrivateKey, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, `\n`, "\n"))
	if !strings.Contains(s, "-----BEGIN") {
		s = "-----BEGIN PRIVATE KEY-----\n" + strings.Join(strings.Fields(s), "\n") + "\n-----END PRIVATE KEY-----"
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("apple: private key is not valid PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apple: parsing private key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve.Params().BitSize != 256 {
		return nil, errors.New("apple: private key must be an EC P-256 key")
	}
	return ec, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// SignToken builds the ES256 developer token.
func SignToken(teamID, keyID string, key *ecdsa.PrivateKey, now time.Time, ttl time.Duration) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": keyID, "typ": "JWT"})
	pl, _ := json.Marshal(map[string]any{"iss": teamID, "iat": now.Unix(), "exp": now.Add(ttl).Unix()})
	signing := b64(hdr) + "." + b64(pl)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS wants fixed-width r||s, not ASN.1
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64(sig), nil
}

// VerifyToken checks a token's signature and returns its claims (tests, and a
// sanity check an admin endpoint could use).
func VerifyToken(tok string, pub *ecdsa.PublicKey) (header, claims map[string]any, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("apple: malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, nil, errors.New("apple: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, nil, errors.New("apple: signature does not verify")
	}
	for i, dst := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(raw, dst) != nil {
			return nil, nil, errors.New("apple: bad token section")
		}
	}
	return header, claims, nil
}

func (c *Client) developerToken(ctx context.Context, force bool) (string, error) {
	team, kid, pk, err := c.cfg.Credentials(ctx)
	if err != nil {
		if errors.Is(err, providers.ErrNotConfigured) {
			return "", err
		}
		return "", fmt.Errorf("apple: reading credentials: %w", err)
	}
	if team == "" || kid == "" || strings.TrimSpace(pk) == "" {
		return "", fmt.Errorf("apple: team id, key id and private key must all be set: %w", providers.ErrNotConfigured)
	}
	cred := team + "\x00" + kid + "\x00" + pk
	now := c.cfg.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.forCred == cred && now.Add(tokenRefresh).Before(c.expires) {
		return c.token, nil
	}
	key, err := ParsePrivateKey(pk)
	if err != nil {
		return "", fmt.Errorf("%v: %w", err, providers.ErrNotConfigured)
	}
	tok, err := SignToken(team, kid, key, now, tokenTTL)
	if err != nil {
		return "", err
	}
	c.token, c.forCred, c.expires = tok, cred, now.Add(tokenTTL)
	return tok, nil
}

// ---- wire types ----

type artwork struct {
	URL    string `json:"url"` // template with {w} and {h}
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type resource struct {
	ID         string `json:"id"`
	Attributes struct {
		Name        string   `json:"name"`
		ArtistName  string   `json:"artistName"`
		AlbumName   string   `json:"albumName"`
		DurationMs  int      `json:"durationInMillis"`
		ISRC        string   `json:"isrc"`
		ReleaseDate string   `json:"releaseDate"`
		TrackNumber int      `json:"trackNumber"`
		DiscNumber  int      `json:"discNumber"`
		TrackCount  int      `json:"trackCount"` // albums
		UPC         string   `json:"upc"`        // albums
		Artwork     *artwork `json:"artwork"`
	} `json:"attributes"`
	Relationships struct {
		Albums  struct{ Data []resource } `json:"albums"`
		Artists struct{ Data []resource } `json:"artists"`
	} `json:"relationships"`
}

func (r resource) toTrack() providers.Track {
	a := r.Attributes
	t := providers.Track{
		Provider: providers.AppleMusic, ProviderID: r.ID, Title: a.Name, DurationMs: a.DurationMs,
	}
	for _, ar := range r.Relationships.Artists.Data {
		name := ar.Attributes.Name
		if name == "" {
			name = a.ArtistName
		}
		t.Artists = append(t.Artists, providers.Artist{ProviderID: ar.ID, Name: name})
	}
	if len(t.Artists) == 0 && a.ArtistName != "" {
		t.Artists = []providers.Artist{{Name: a.ArtistName}}
	}
	if a.AlbumName != "" {
		ref := providers.ReleaseRef{Title: a.AlbumName, Date: a.ReleaseDate, DateIsTrack: a.ReleaseDate != "", Disc: a.DiscNumber, Position: a.TrackNumber}
		if al := r.Relationships.Albums.Data; len(al) > 0 {
			ref.ProviderID = al[0].ID
			if n := al[0].Attributes.ArtistName; n != "" { // Apple names the album artist, with no id
				ref.Artists = []providers.Artist{{Name: n}}
			}
		}
		t.Releases = []providers.ReleaseRef{ref}
	}
	if a.ISRC != "" {
		t.ISRCs = []string{strings.ToUpper(a.ISRC)}
	}
	return t
}

func artworkRefs(a *artwork, kind string) []providers.ImageRef {
	if a == nil || a.URL == "" {
		return nil
	}
	var out []providers.ImageRef
	for _, size := range []int{1000, 600, 100} {
		if a.Width > 0 && size > a.Width {
			size = a.Width // never advertise more than the original
		}
		u := strings.NewReplacer("{w}", strconv.Itoa(size), "{h}", strconv.Itoa(size)).Replace(a.URL)
		if len(out) > 0 && out[len(out)-1].Width == size {
			continue
		}
		out = append(out, providers.ImageRef{URL: u, Width: size, Height: size, Kind: kind})
	}
	return out
}

// ---- Provider methods ----

func (c *Client) cat(path string) string {
	return "/catalog/" + url.PathEscape(c.cfg.Storefront) + path
}

const songInclude = "include=albums,artists"

func (c *Client) SearchTrack(ctx context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	if q.ISRC != "" && q.Title == "" {
		return c.LookupISRC(ctx, q.ISRC)
	}
	term := strings.Join(strings.Fields(q.Artist+" "+q.Title), " ")
	if term == "" {
		return nil, errors.New("apple: empty query")
	}
	limit := q.Limit
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	v := url.Values{"term": {term}, "types": {"songs"}, "limit": {strconv.Itoa(limit)}}
	var out struct {
		Results struct {
			Songs struct{ Data []resource } `json:"songs"`
		} `json:"results"`
	}
	if err := c.get(ctx, c.cat("/search?"+v.Encode()+"&"+songInclude), &out); err != nil {
		return nil, err
	}
	return tracks(out.Results.Songs.Data), nil
}

func tracks(rs []resource) []providers.Track {
	out := make([]providers.Track, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.toTrack())
	}
	return out
}

func (c *Client) GetTrack(ctx context.Context, id string) (*providers.Track, error) {
	if !digits(id) {
		return nil, providers.ErrNotFound
	}
	var out struct{ Data []resource }
	if err := c.get(ctx, c.cat("/songs/"+id+"?"+songInclude), &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, providers.ErrNotFound
	}
	t := out.Data[0].toTrack()
	return &t, nil
}

// LookupISRC returns at most one track: Apple lists one song per release, all
// sharing the ISRC, and the first is Apple's own preferred ordering. Several
// copies are one recording, not an ambiguity for the matcher.
func (c *Client) LookupISRC(ctx context.Context, isrc string) ([]providers.Track, error) {
	isrc = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(isrc), "-", ""))
	if len(isrc) != 12 {
		return nil, providers.ErrNotFound
	}
	var out struct{ Data []resource }
	if err := c.get(ctx, c.cat("/songs?filter[isrc]="+url.QueryEscape(isrc)+"&"+songInclude), &out); err != nil {
		return nil, err
	}
	for _, r := range out.Data {
		if strings.EqualFold(r.Attributes.ISRC, isrc) {
			return []providers.Track{r.toTrack()}, nil
		}
	}
	return nil, nil
}

// GetAlbum returns an album by its Apple Music id, with its barcode (UPC), its
// own release date and its track count. A song's releaseDate can be the
// original release of the track (a compilation, a reissue), so the album's own
// date is the one to compare with another catalogue's album.
func (c *Client) GetAlbum(ctx context.Context, id string) (*providers.Album, error) {
	if !digits(id) {
		return nil, providers.ErrNotFound
	}
	var out struct{ Data []resource }
	if err := c.get(ctx, c.cat("/albums/"+id), &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, providers.ErrNotFound
	}
	a := out.Data[0]
	return &providers.Album{ProviderID: a.ID, Title: a.Attributes.Name, Date: a.Attributes.ReleaseDate,
		UPC: a.Attributes.UPC, TotalTracks: a.Attributes.TrackCount}, nil
}

// GetArtwork takes an Apple Music album id.
func (c *Client) GetArtwork(ctx context.Context, albumID string) ([]providers.ImageRef, error) {
	if !digits(albumID) {
		return nil, providers.ErrNotFound
	}
	var out struct{ Data []resource }
	if err := c.get(ctx, c.cat("/albums/"+albumID), &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, providers.ErrNotFound
	}
	return artworkRefs(out.Data[0].Attributes.Artwork, "front"), nil
}

// GetArtistImage takes an Apple Music artist id. Not every artist has artwork.
func (c *Client) GetArtistImage(ctx context.Context, artistID string) ([]providers.ImageRef, error) {
	if !digits(artistID) {
		return nil, providers.ErrNotFound
	}
	var out struct{ Data []resource }
	if err := c.get(ctx, c.cat("/artists/"+artistID), &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, providers.ErrNotFound
	}
	return artworkRefs(out.Data[0].Attributes.Artwork, "artist"), nil
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

// ---- HTTP ----

var errRetryAuth = errors.New("retry with fresh token")

func (c *Client) get(ctx context.Context, path string, dst any) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.developerToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
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
				ra := 5
				if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
					ra = s
				}
				return &providers.RateLimitedError{Provider: providers.AppleMusic, RetryAfter: ra}
			case resp.StatusCode == http.StatusUnauthorized:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				if attempt == 0 {
					return errRetryAuth // re-sign once, in case of clock skew or a stale token
				}
				return fmt.Errorf("apple: developer token rejected (HTTP 401): %w", providers.ErrNotConfigured)
			case resp.StatusCode != http.StatusOK:
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				return fmt.Errorf("apple: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			}
			return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(dst)
		}()
		if errors.Is(err, errRetryAuth) {
			continue
		}
		return err
	}
}
