package musicbrainz

import (
	"context"
	"strings"
	"sync"
	"time"

	"younithm/internal/providers"
)

// ContactFunc returns the contact (email or URL) for the User-Agent, or
// providers.ErrNotConfigured when there is none yet.
type ContactFunc func(ctx context.Context) (string, error)

// Dynamic is a MusicBrainz provider whose contact is read on every call, so it
// can be set or changed at runtime (the same way Spotify and Apple keys are).
// Until a contact exists every call returns providers.ErrNotConfigured, which
// the matcher skips quietly. One request limiter is shared by every client it
// builds, so changing the contact never lets requests exceed 1 per second.
type Dynamic struct {
	contact ContactFunc
	base    Config

	mu     sync.Mutex
	cur    *Client
	curFor string
}

var (
	_ providers.Provider      = (*Dynamic)(nil)
	_ providers.ISRCLookup    = (*Dynamic)(nil)
	_ providers.BarcodeLookup = (*Dynamic)(nil)
	_ providers.TrackIDLookup = (*Dynamic)(nil)

	_ providers.ReleaseTracksLookup = (*Dynamic)(nil)
	_ providers.LooseSearch         = (*Dynamic)(nil)
)

// NewDynamic builds the provider. base supplies everything but the contact.
func NewDynamic(contact ContactFunc, base Config) *Dynamic {
	d := &Dynamic{contact: contact, base: base}
	if d.base.Limiter == nil {
		iv := base.Interval
		if iv <= 0 {
			iv = time.Second // MusicBrainz: at most one request per second
		}
		d.base.Limiter = providers.NewLimiter(iv)
	}
	return d
}

func (d *Dynamic) Name() string { return providers.MusicBrainz }

func (d *Dynamic) client(ctx context.Context) (*Client, error) {
	c, err := d.contact(ctx)
	if err != nil {
		return nil, err
	}
	c = strings.TrimSpace(c)
	if c == "" {
		return nil, providers.ErrNotConfigured
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur != nil && d.curFor == c {
		return d.cur, nil
	}
	cfg := d.base
	cfg.Contact = c
	cl, err := New(cfg)
	if err != nil {
		return nil, err
	}
	d.cur, d.curFor = cl, c
	return cl, nil
}

// Configured reports whether a contact is available right now.
func (d *Dynamic) Configured(ctx context.Context) bool {
	_, err := d.client(ctx)
	return err == nil
}

// UserAgent returns the header value currently sent, or "" when not configured.
func (d *Dynamic) UserAgent(ctx context.Context) string {
	c, err := d.client(ctx)
	if err != nil {
		return ""
	}
	return c.UserAgent()
}

func (d *Dynamic) SearchTrack(ctx context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.SearchTrack(ctx, q)
}

func (d *Dynamic) GetTrack(ctx context.Context, id string) (*providers.Track, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetTrack(ctx, id)
}

func (d *Dynamic) LookupISRC(ctx context.Context, isrc string) ([]providers.Track, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.LookupISRC(ctx, isrc)
}

func (d *Dynamic) LookupTrackID(ctx context.Context, id string) (*providers.Track, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.LookupTrackID(ctx, id)
}

func (d *Dynamic) LookupReleaseTracks(ctx context.Context, id string) ([]providers.Track, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.LookupReleaseTracks(ctx, id)
}

func (d *Dynamic) SearchLoose(ctx context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.SearchLoose(ctx, q)
}

func (d *Dynamic) LookupBarcode(ctx context.Context, barcode string) ([]providers.Release, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.LookupBarcode(ctx, barcode)
}

func (d *Dynamic) GetArtwork(ctx context.Context, id string) ([]providers.ImageRef, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetArtwork(ctx, id)
}

func (d *Dynamic) GetArtistImage(ctx context.Context, id string) ([]providers.ImageRef, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetArtistImage(ctx, id)
}
