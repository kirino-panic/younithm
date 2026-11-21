// Package musicbrainz implements providers.Provider (and ISRCLookup) on top of
// the MusicBrainz web service v2 and the Cover Art Archive.
//
// Rules followed (see docs/providers.md): at most one request
// per second on average, a User-Agent that identifies the app and a contact,
// JSON via fmt=json, and backing off on 503/429.
package musicbrainz

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
	"unicode"

	"younithm/internal/config"
	"younithm/internal/providers"
	"younithm/internal/version"
)

const (
	DefaultBaseURL  = "https://musicbrainz.org/ws/2"
	DefaultCoverURL = "https://coverartarchive.org"
	maxBody         = 8 << 20
)

// Config configures the client. Contact is required: MusicBrainz asks that
// every client can be contacted (email or URL), and blocks anonymous ones.
type Config struct {
	AppName    string // default "younithm"
	AppVersion string // default version.Version
	Contact    string // email or URL; required
	BaseURL    string // default DefaultBaseURL
	CoverURL   string // default DefaultCoverURL
	// Interval between requests; default 1s. Do not go lower against the
	// public server.
	Interval   time.Duration
	HTTPClient *http.Client
	Limiter    *providers.Limiter // optional; share one per outbound IP
}

type Client struct {
	cfg Config
	ua  string
	hc  *http.Client
	lim *providers.Limiter

	relMu    sync.Mutex
	relCache map[string]relTracksEntry
}

var (
	_ providers.Provider      = (*Client)(nil)
	_ providers.ISRCLookup    = (*Client)(nil)
	_ providers.BarcodeLookup = (*Client)(nil)

	_ providers.ReleaseTracksLookup = (*Client)(nil)
	_ providers.LooseSearch         = (*Client)(nil)
)

// NewFromConfig builds a Client whose User-Agent contact comes from the app
// configuration (MUSICBRAINZ_CONTACT). base may pre-set other fields; a
// Contact already in base wins. Fails with providers.ErrNotConfigured when no
// contact is configured anywhere.
func NewFromConfig(app config.Config, base Config) (*Client, error) {
	if strings.TrimSpace(base.Contact) == "" {
		base.Contact = app.MusicBrainzContact
	}
	return New(base)
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Contact) == "" {
		return nil, fmt.Errorf("musicbrainz: contact (email or URL) required for User-Agent: %w", providers.ErrNotConfigured)
	}
	if cfg.AppName == "" {
		cfg.AppName = "younithm"
	}
	if cfg.AppVersion == "" {
		cfg.AppVersion = version.Version
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.CoverURL == "" {
		cfg.CoverURL = DefaultCoverURL
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	lim := cfg.Limiter
	if lim == nil {
		lim = providers.NewLimiter(cfg.Interval)
	}
	return &Client{
		cfg: cfg,
		ua:  fmt.Sprintf("%s/%s ( %s )", cfg.AppName, cfg.AppVersion, cfg.Contact),
		hc:  hc,
		lim: lim,

		relCache: map[string]relTracksEntry{},
	}, nil
}

func (c *Client) Name() string { return providers.MusicBrainz }

// UserAgent returns the header value sent on every request.
func (c *Client) UserAgent() string { return c.ua }

// ---- wire types ----

type mbArtistCredit struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
	Artist     struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		SortName string `json:"sort-name"`
	} `json:"artist"`
}

type mbRelease struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Status       string           `json:"status"`
	Date         string           `json:"date"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"` // the album artists
	ReleaseGroup struct {
		ID          string `json:"id"`
		PrimaryType string `json:"primary-type"`
	} `json:"release-group"`
	Media []struct {
		Position int `json:"position"`
		Track    []struct {
			Number string `json:"number"`
		} `json:"track"`
	} `json:"media"`
}

type mbRecording struct {
	ID           string           `json:"id"`
	Score        int              `json:"score"`
	Title        string           `json:"title"`
	Length       int              `json:"length"`
	Disamb       string           `json:"disambiguation"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
	Releases     []mbRelease      `json:"releases"`
	ISRCs        []string         `json:"isrcs"`
}

// credits maps a MusicBrainz artist credit to provider artists, keeping the
// credit order, join phrases and the names artists are credited as.
func credits(acs []mbArtistCredit) []providers.Artist {
	var out []providers.Artist
	for _, ac := range acs {
		name := ac.Artist.Name
		if name == "" {
			name = ac.Name
		}
		credited := ""
		if ac.Name != "" && ac.Name != name {
			credited = ac.Name
		}
		out = append(out, providers.Artist{
			ProviderID: ac.Artist.ID, Name: name, SortName: ac.Artist.SortName, JoinPhrase: ac.JoinPhrase, CreditedName: credited,
		})
	}
	return out
}

func (r mbRecording) toTrack() providers.Track {
	t := providers.Track{
		Provider:       providers.MusicBrainz,
		ProviderID:     r.ID,
		Title:          r.Title,
		DurationMs:     r.Length,
		ISRCs:          r.ISRCs,
		Score:          r.Score,
		Disambiguation: r.Disamb,
	}
	t.Artists = credits(r.ArtistCredit)
	for _, rel := range r.Releases {
		ref := providers.ReleaseRef{
			ProviderID: rel.ID, Title: rel.Title, Date: rel.Date,
			Type: strings.ToLower(rel.ReleaseGroup.PrimaryType), Status: strings.ToLower(rel.Status),
			GroupID: rel.ReleaseGroup.ID, Artists: credits(rel.ArtistCredit),
		}
		t.Releases = append(t.Releases, ref)
	}
	return t
}

// ---- Provider methods ----

// SearchTrack runs a Lucene recording search.
func (c *Client) SearchTrack(ctx context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	if q.ISRC != "" && q.Title == "" {
		return c.LookupISRC(ctx, q.ISRC)
	}
	query := BuildQuery(q)
	if query == "" {
		return nil, errors.New("musicbrainz: empty query")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	return c.searchRecordings(ctx, query, limit)
}

// SearchLoose runs the easier recording search: each field that is set becomes
// a set of words (all of them, in any order) instead of an exact phrase.
func (c *Client) SearchLoose(ctx context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	query := BuildLooseQuery(q)
	if query == "" {
		return nil, errors.New("musicbrainz: empty query")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 25
	}
	return c.searchRecordings(ctx, query, limit)
}

func (c *Client) searchRecordings(ctx context.Context, query string, limit int) ([]providers.Track, error) {
	v := url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}, "fmt": {"json"}}
	var out struct {
		Recordings []mbRecording `json:"recordings"`
	}
	if err := c.get(ctx, c.cfg.BaseURL+"/recording?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	tracks := make([]providers.Track, 0, len(out.Recordings))
	for _, r := range out.Recordings {
		tracks = append(tracks, r.toTrack())
	}
	return tracks, nil
}

// relTracksTTL and relTracksMax bound the small cache of release tracklists: a
// batch of listens usually comes from a few dozen albums, so each is fetched
// once, not once per track.
const (
	relTracksTTL = 30 * time.Minute
	relTracksMax = 256
)

type relTracksEntry struct {
	tracks []providers.Track // nil with notFound set
	gone   bool
	at     time.Time
}

type mbTracklist struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Status       string           `json:"status"`
	Date         string           `json:"date"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
	ReleaseGroup struct {
		ID          string `json:"id"`
		PrimaryType string `json:"primary-type"`
	} `json:"release-group"`
	Media []struct {
		Position int `json:"position"`
		Tracks   []struct {
			Position     int              `json:"position"`
			Title        string           `json:"title"`
			Length       int              `json:"length"`
			ArtistCredit []mbArtistCredit `json:"artist-credit"`
			Recording    struct {
				ID           string           `json:"id"`
				Title        string           `json:"title"`
				Length       int              `json:"length"`
				Disamb       string           `json:"disambiguation"`
				ArtistCredit []mbArtistCredit `json:"artist-credit"`
				ISRCs        []string         `json:"isrcs"`
			} `json:"recording"`
		} `json:"tracks"`
	} `json:"media"`
}

// LookupReleaseTracks returns the tracks of a release as recordings, each
// carrying that release in its release list. One request, then cached.
func (c *Client) LookupReleaseTracks(ctx context.Context, releaseID string) ([]providers.Track, error) {
	if !validID(releaseID) {
		return nil, providers.ErrNotFound
	}
	key := strings.ToLower(releaseID)
	c.relMu.Lock()
	e, ok := c.relCache[key]
	c.relMu.Unlock()
	if ok && time.Since(e.at) < relTracksTTL {
		if e.gone {
			return nil, providers.ErrNotFound
		}
		return append([]providers.Track(nil), e.tracks...), nil
	}
	var r mbTracklist
	u := c.cfg.BaseURL + "/release/" + key + "?inc=recordings+artist-credits+isrcs+release-groups&fmt=json"
	if err := c.get(ctx, u, &r); err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			c.putRelTracks(key, relTracksEntry{gone: true, at: time.Now()})
		}
		return nil, err
	}
	ref := providers.ReleaseRef{
		ProviderID: r.ID, Title: r.Title, Date: r.Date, Type: strings.ToLower(r.ReleaseGroup.PrimaryType),
		Status: strings.ToLower(r.Status), GroupID: r.ReleaseGroup.ID, Artists: credits(r.ArtistCredit),
	}
	var tracks []providers.Track
	for _, m := range r.Media {
		for _, t := range m.Tracks {
			if t.Recording.ID == "" {
				continue
			}
			ac := t.Recording.ArtistCredit
			if len(ac) == 0 {
				ac = t.ArtistCredit
			}
			if len(ac) == 0 {
				ac = r.ArtistCredit
			}
			length := t.Recording.Length
			if length == 0 {
				length = t.Length
			}
			here := ref
			here.Disc, here.Position = m.Position, t.Position
			tracks = append(tracks, providers.Track{
				Provider: providers.MusicBrainz, ProviderID: t.Recording.ID, Title: t.Recording.Title,
				DurationMs: length, Artists: credits(ac), Releases: []providers.ReleaseRef{here}, ISRCs: t.Recording.ISRCs,
				Disambiguation: t.Recording.Disamb,
			})
		}
	}
	c.putRelTracks(key, relTracksEntry{tracks: tracks, at: time.Now()})
	return append([]providers.Track(nil), tracks...), nil
}

func (c *Client) putRelTracks(key string, e relTracksEntry) {
	c.relMu.Lock()
	defer c.relMu.Unlock()
	if len(c.relCache) >= relTracksMax {
		for k := range c.relCache { // full: drop everything, it is only a cache
			delete(c.relCache, k)
		}
	}
	c.relCache[key] = e
}

const lookupInc = "artists+releases+isrcs+release-groups"

// GetTrack looks a recording up by MBID.
func (c *Client) GetTrack(ctx context.Context, mbid string) (*providers.Track, error) {
	if !validID(mbid) {
		return nil, providers.ErrNotFound
	}
	var r mbRecording
	u := c.cfg.BaseURL + "/recording/" + mbid + "?inc=" + lookupInc + "&fmt=json"
	if err := c.get(ctx, u, &r); err != nil {
		return nil, err
	}
	t := r.toTrack()
	return &t, nil
}

// LookupTrackID resolves a MusicBrainz release-track id to its recording. The
// two look alike (both are UUIDs) and histories mix them up, so a recording
// lookup that returns "not found" may simply have been given a track id. One
// search (tid:) answers it, with the recording's full release list and ISRCs.
func (c *Client) LookupTrackID(ctx context.Context, id string) (*providers.Track, error) {
	if !validID(id) {
		return nil, providers.ErrNotFound
	}
	var out struct {
		Recordings []mbRecording `json:"recordings"`
	}
	v := url.Values{"query": {"tid:" + strings.ToLower(id)}, "limit": {"2"}, "fmt": {"json"}}
	if err := c.get(ctx, c.cfg.BaseURL+"/recording?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	// A track id belongs to exactly one recording; anything else is no answer.
	if len(out.Recordings) != 1 {
		return nil, providers.ErrNotFound
	}
	t := out.Recordings[0].toTrack()
	return &t, nil
}

// LookupISRC returns every recording carrying the ISRC. ISRCs are evidence,
// not proof, so callers must expect several results.
func (c *Client) LookupISRC(ctx context.Context, isrc string) ([]providers.Track, error) {
	isrc = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(isrc), "-", ""))
	if len(isrc) != 12 {
		return nil, providers.ErrNotFound
	}
	var out struct {
		Recordings []mbRecording `json:"recordings"`
	}
	u := c.cfg.BaseURL + "/isrc/" + isrc + "?inc=artists+releases+isrcs&fmt=json"
	if err := c.get(ctx, u, &out); err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	tracks := make([]providers.Track, 0, len(out.Recordings))
	for _, r := range out.Recordings {
		tracks = append(tracks, r.toTrack())
	}
	return tracks, nil
}

// GetArtwork queries the Cover Art Archive for a release MBID.
func (c *Client) GetArtwork(ctx context.Context, releaseMBID string) ([]providers.ImageRef, error) {
	if !validID(releaseMBID) {
		return nil, providers.ErrNotFound
	}
	var out struct {
		Images []struct {
			Front      bool              `json:"front"`
			Back       bool              `json:"back"`
			Types      []string          `json:"types"`
			Image      string            `json:"image"`
			Thumbnails map[string]string `json:"thumbnails"`
		} `json:"images"`
	}
	if err := c.get(ctx, c.cfg.CoverURL+"/release/"+releaseMBID, &out); err != nil {
		return nil, err
	}
	var imgs []providers.ImageRef
	for _, im := range out.Images {
		kind := "other"
		switch {
		case im.Front:
			kind = "front"
		case im.Back:
			kind = "back"
		case len(im.Types) > 0:
			kind = strings.ToLower(im.Types[0])
		}
		ref := providers.ImageRef{URL: im.Image, Kind: kind}
		// The original can be many megabytes; the 500px thumbnail is what a page needs.
		if t := im.Thumbnails["500"]; t != "" {
			ref = providers.ImageRef{URL: t, Width: 500, Height: 500, Kind: kind}
		}
		imgs = append(imgs, ref)
	}
	// Front covers first, stable otherwise.
	front := imgs[:0:0]
	rest := imgs[:0:0]
	for _, i := range imgs {
		if i.Kind == "front" {
			front = append(front, i)
		} else {
			rest = append(rest, i)
		}
	}
	return append(front, rest...), nil
}

// maxBarcodeReleases caps how many releases sharing one barcode are fetched in
// full (each is one more request).
const maxBarcodeReleases = 3

type mbFullRelease struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Date    string `json:"date"`
	Barcode string `json:"barcode"`
	Media   []struct {
		Position int `json:"position"`
		Tracks   []struct {
			Position  int    `json:"position"`
			Title     string `json:"title"`
			Length    int    `json:"length"`
			Recording struct {
				ID     string   `json:"id"`
				Title  string   `json:"title"`
				Length int      `json:"length"`
				ISRCs  []string `json:"isrcs"`
			} `json:"recording"`
		} `json:"tracks"`
	} `json:"media"`
}

func (r mbFullRelease) toRelease() providers.Release {
	out := providers.Release{ProviderID: r.ID, Title: r.Title, Date: r.Date, Barcode: r.Barcode}
	for _, m := range r.Media {
		for _, t := range m.Tracks {
			length := t.Recording.Length
			if length == 0 {
				length = t.Length
			}
			out.Tracks = append(out.Tracks, providers.ReleaseTrack{
				Disc: m.Position, Position: t.Position, RecordingID: t.Recording.ID,
				Title: t.Recording.Title, DurationMs: length, ISRCs: t.Recording.ISRCs,
			})
		}
	}
	return out
}

// BarcodeForms returns the spellings a barcode can take in a catalogue: as
// given, as a 12-digit UPC-A and as a 13-digit EAN-13 (Spotify and Apple often
// pad with leading zeros, MusicBrainz stores whichever the editor typed). Nil
// when it is not a barcode at all.
func BarcodeForms(barcode string) []string {
	var digits strings.Builder
	for _, r := range barcode {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	trim := strings.TrimLeft(digits.String(), "0")
	if len(trim) < 8 || len(trim) > 13 {
		return nil
	}
	if len(trim) == 13 { // a real EAN-13 (no leading zero): it has only one spelling
		return []string{trim}
	}
	for len(trim) < 12 {
		trim = "0" + trim
	}
	return []string{trim, "0" + trim}
}

// LookupBarcode finds the releases that carry the barcode and returns their
// tracklists: one search and one lookup per release (usually one), at the usual
// one request a second. A release is kept only if its barcode really equals the
// one asked for.
func (c *Client) LookupBarcode(ctx context.Context, barcode string) ([]providers.Release, error) {
	forms := BarcodeForms(barcode)
	if forms == nil {
		return nil, nil
	}
	q := "barcode:(" + strings.Join(forms, " OR ") + ")"
	var found struct {
		Releases []struct {
			ID      string `json:"id"`
			Barcode string `json:"barcode"`
		} `json:"releases"`
	}
	if err := c.get(ctx, c.cfg.BaseURL+"/release?query="+url.QueryEscape(q)+"&limit=5&fmt=json", &found); err != nil {
		return nil, err
	}
	var out []providers.Release
	for _, h := range found.Releases {
		if len(out) == maxBarcodeReleases {
			break
		}
		if !validID(h.ID) || !barcodeIn(h.Barcode, forms) {
			continue
		}
		var r mbFullRelease
		if err := c.get(ctx, c.cfg.BaseURL+"/release/"+h.ID+"?inc=recordings+isrcs&fmt=json", &r); err != nil {
			if errors.Is(err, providers.ErrNotFound) {
				continue
			}
			return out, err
		}
		if barcodeIn(r.Barcode, forms) {
			out = append(out, r.toRelease())
		}
	}
	return out, nil
}

func barcodeIn(b string, forms []string) bool {
	t := strings.TrimLeft(b, "0")
	for _, f := range forms {
		if t != "" && t == strings.TrimLeft(f, "0") {
			return true
		}
	}
	return false
}

// GetArtistImage: MusicBrainz does not host artist images.
func (c *Client) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}

// ---- HTTP ----

func (c *Client) get(ctx context.Context, u string, dst any) error {
	if err := c.lim.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return providers.ErrNotFound
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		ra := 5
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ra = s
		}
		c.lim.Penalize(time.Duration(ra) * time.Second)
		return &providers.RateLimitedError{Provider: providers.MusicBrainz, RetryAfter: ra}
	case resp.StatusCode != http.StatusOK:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("musicbrainz: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(dst)
}

func validID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'):
			return false
		}
	}
	return true
}

// BuildQuery builds the Lucene query for a recording search.
func BuildQuery(q providers.TrackQuery) string {
	var parts []string
	if q.Title != "" {
		parts = append(parts, `recording:"`+escapePhrase(q.Title)+`"`)
	}
	if q.Artist != "" {
		parts = append(parts, `artist:"`+escapePhrase(q.Artist)+`"`)
	}
	if q.Album != "" {
		parts = append(parts, `release:"`+escapePhrase(q.Album)+`"`)
	}
	return strings.Join(parts, " AND ")
}

// BuildLooseQuery builds the easier Lucene query: every field that is set
// becomes the words of its value (all required, in any order). Only letters,
// digits and apostrophes survive, so no Lucene syntax can leak in from a title;
// the apostrophe stays because the index keeps "don't" as one word.
func BuildLooseQuery(q providers.TrackQuery) string {
	var parts []string
	add := func(field, value string) {
		words := strings.FieldsFunc(value, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
		})
		if len(words) > 0 {
			parts = append(parts, field+":("+strings.Join(words, " AND ")+")")
		}
	}
	add("recording", q.Title)
	add("artist", q.Artist)
	add("release", q.Album)
	return strings.Join(parts, " AND ")
}

// escapePhrase escapes characters special inside a quoted Lucene phrase.
func escapePhrase(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
