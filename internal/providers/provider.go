// Package providers defines the metadata provider abstraction
// (docs/providers.md) and a small ordered registry. Concrete providers live in
// subpackages (musicbrainz now; spotify, apple, itunes later).
//
// Providers are stateless network clients. They never touch the database;
// caching (providers/snapshot) and linking (providers/match) sit above them.
package providers

import (
	"context"
	"errors"
	"fmt"
)

// Provider names, used as external_link.provider and provider_snapshot.provider.
const (
	MusicBrainz = "musicbrainz"
	Spotify     = "spotify"
	AppleMusic  = "apple"
	ITunes      = "itunes"
)

// Errors providers should return so callers can react uniformly.
var (
	// ErrNotFound: the provider has no such entity. Safe to negative-cache.
	ErrNotFound = errors.New("providers: not found")
	// ErrUnsupported: the provider cannot do this operation at all.
	ErrUnsupported = errors.New("providers: operation not supported")
	// ErrNotConfigured: credentials / contact info missing.
	ErrNotConfigured = errors.New("providers: not configured")
)

// RateLimitedError is returned when the provider asked us to back off
// (HTTP 429/503). The job queue should retry after RetryAfter.
type RateLimitedError struct {
	Provider   string
	RetryAfter int // seconds; 0 if unknown
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("%s: rate limited (retry after %ds)", e.Provider, e.RetryAfter)
}

// TrackQuery is what we know about a play, used to search a provider.
type TrackQuery struct {
	Artist     string
	Title      string
	Album      string
	DurationMs int // 0 = unknown
	ISRC       string
	Limit      int // 0 = provider default
}

// Artist is a provider-side artist credit.
type Artist struct {
	ProviderID string // e.g. MBID
	Name       string
	SortName   string
	// JoinPhrase is the text that follows this credit when the credits are
	// displayed (" & ", " feat. "), where the provider reports one.
	JoinPhrase string
	// CreditedName is the name the artist is credited as here when it differs
	// from Name (MusicBrainz "artist as credited"); "" when it doesn't.
	CreditedName string
}

// ReleaseRef is a provider-side release the recording appears on.
type ReleaseRef struct {
	ProviderID string
	Title      string
	Date       string // as given: YYYY, YYYY-MM or YYYY-MM-DD
	// DateIsTrack says Date is the track's own release date, not the album's:
	// Apple Music and iTunes report a song's date (a compilation or reissue
	// carries the track's original one), where Spotify and MusicBrainz report the
	// release's. Compare such a date with another catalogue's album only after
	// the album's own date is known (AlbumLookup).
	DateIsTrack bool
	Type        string // album, single, ...
	Status      string // official, promotion, bootleg...
	Disc        int
	Position    int
	// Artists are the album artists, when the provider reports them (ids where
	// it has them). GroupID is the MusicBrainz release-group id.
	Artists []Artist
	GroupID string
}

// Track is a normalised recording from a provider. Raw provider JSON is kept
// separately in provider_snapshot; this is only the common subset.
type Track struct {
	Provider   string
	ProviderID string
	Title      string
	DurationMs int
	Artists    []Artist
	Releases   []ReleaseRef
	ISRCs      []string
	// Disambiguation is MusicBrainz's comment that tells same-named
	// recordings apart ("live", "radio edit"). Empty elsewhere.
	Disambiguation string `json:",omitempty"`
	// Score is the provider's own relevance score, 0..100, when searching.
	// 0 when not applicable (direct lookups).
	Score int
}

// ImageRef points at an image a provider can serve.
type ImageRef struct {
	URL    string
	Width  int
	Height int
	// Kind is "front", "back", "artist", ... provider-defined but stable.
	Kind string
}

// Provider is the one interface every metadata source implements.
// Operations a provider cannot do return ErrUnsupported. Implementations must
// be safe for concurrent use and must honour ctx cancellation.
type Provider interface {
	Name() string
	SearchTrack(ctx context.Context, q TrackQuery) ([]Track, error)
	GetTrack(ctx context.Context, providerID string) (*Track, error)
	// GetArtwork returns artwork candidates for a release id as the provider
	// understands it, best first.
	GetArtwork(ctx context.Context, providerID string) ([]ImageRef, error)
	GetArtistImage(ctx context.Context, providerID string) ([]ImageRef, error)
}

// ISRCLookup is an optional capability: exact recording lookup by ISRC.
// The matching ladder uses it when the provider implements it.
type ISRCLookup interface {
	LookupISRC(ctx context.Context, isrc string) ([]Track, error)
}

// TrackIDLookup is an optional capability: resolve an id that names a track on
// one release (MusicBrainz's release-track ids, which histories such as Koito
// and Last.fm often submit where a recording id is expected) to the recording
// it belongs to. It returns ErrNotFound when the id is no track either.
type TrackIDLookup interface {
	LookupTrackID(ctx context.Context, id string) (*Track, error)
}

// ReleaseTracksLookup is an optional capability: the tracks of one release, by
// the release's own id, each as a Track (its recording id, title, credits and
// the release it is on). A listen whose history named its release can be
// matched against that tracklist directly instead of searching by name. It
// returns ErrNotFound when the release does not exist.
type ReleaseTracksLookup interface {
	LookupReleaseTracks(ctx context.Context, releaseID string) ([]Track, error)
}

// LooseSearch is an optional capability: a search that is easier to satisfy than
// SearchTrack. Only the fields that are set are used, and each is matched as a
// set of words, not an exact phrase, so a credit written "A & B & C" or a
// title carrying a "(feat. X)" the catalogue leaves out can still be found. It
// returns more, and noisier, results: the matcher scores every one of them.
type LooseSearch interface {
	SearchLoose(ctx context.Context, q TrackQuery) ([]Track, error)
}

// Entity kinds the batch lookups below take.
const (
	KindArtist    = "artist"
	KindAlbum     = "album"
	KindRecording = "recording"
)

// LocalizedNames is an optional capability: the names of many catalogue ids at
// once, worded in a language (a tag such as "ja" or "en-US"). The answer maps
// each id the catalogue knows to its name; an id it does not sell or know is
// left out. BatchSize is the most ids one call may carry for kind (0 = the
// kind is not supported). Authoritative reports whether the catalogue states
// the answer is in lang (Apple: a storefront that supports it) rather than
// possibly falling back to another wording (Spotify); ErrUnsupported when no
// request can be made in lang.
type LocalizedNames interface {
	NamesIn(ctx context.Context, kind string, ids []string, lang string) (names map[string]string, authoritative bool, err error)
	BatchSize(kind string) int
}

// LocalName is an artist's name in one language, as a catalogue records it.
// Lang is a tag ("en", "zh-Hant"); Primary marks the catalogue's chosen name
// for that language.
type LocalName struct {
	Lang    string
	Name    string
	Primary bool
}

// ArtistNamesLookup is an optional capability (MusicBrainz): the names by
// language of many artists at once (MaxArtists per call), by the catalogue's
// own ids. An artist the catalogue does not return is left out.
type ArtistNamesLookup interface {
	ArtistNames(ctx context.Context, ids []string) (map[string][]LocalName, error)
	MaxArtists() int
}

// Album is a release as a catalogue lists it: just what album-first matching
// needs, which is the barcode that identifies the release across catalogues.
type Album struct {
	ProviderID  string
	Title       string
	Date        string // as given: YYYY, YYYY-MM or YYYY-MM-DD
	UPC         string // barcode (UPC or EAN), "" when the catalogue has none
	TotalTracks int
}

// AlbumLookup is an optional capability: an album by its own id, with its
// barcode. Used by the high-precision matching mode.
type AlbumLookup interface {
	GetAlbum(ctx context.Context, id string) (*Album, error)
}

// ReleaseTrack is one track of a release's tracklist, by disc and position.
type ReleaseTrack struct {
	Disc, Position int
	RecordingID    string
	Title          string
	DurationMs     int
	ISRCs          []string
}

// Release is a MusicBrainz release with its tracklist.
type Release struct {
	ProviderID string
	Title      string
	Date       string
	Barcode    string
	Tracks     []ReleaseTrack
}

// BarcodeLookup is an optional capability: the releases that carry a barcode,
// with their tracklists. Returns nothing (not an error) when there are none.
type BarcodeLookup interface {
	LookupBarcode(ctx context.Context, barcode string) ([]Release, error)
}

// Registry is an ordered list of providers. Order is the fallback order
// (ordered, with per-field fallback).
type Registry struct {
	list []Provider
}

func NewRegistry(ps ...Provider) *Registry {
	r := &Registry{}
	for _, p := range ps {
		r.Add(p)
	}
	return r
}

// Add appends p, replacing any existing provider of the same name in place.
func (r *Registry) Add(p Provider) {
	for i, e := range r.list {
		if e.Name() == p.Name() {
			r.list[i] = p
			return
		}
	}
	r.list = append(r.list, p)
}

func (r *Registry) All() []Provider { return append([]Provider(nil), r.list...) }

func (r *Registry) Get(name string) (Provider, bool) {
	for _, p := range r.list {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}
