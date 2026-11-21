package providers

import "context"

// When wraps p so it answers only while active reports true; otherwise every
// call returns ErrNotConfigured, which the matcher treats as "skip quietly".
// Used for fallbacks (e.g. the keyless iTunes API only while Apple Music keys
// are absent). The wrapper exposes the plain Provider interface only.
func When(p Provider, active func(ctx context.Context) bool) Provider {
	return &gated{p: p, active: active}
}

type gated struct {
	p      Provider
	active func(ctx context.Context) bool
}

func (g *gated) Name() string { return g.p.Name() }

func (g *gated) ok(ctx context.Context) error {
	if g.active(ctx) {
		return nil
	}
	return ErrNotConfigured
}

func (g *gated) SearchTrack(ctx context.Context, q TrackQuery) ([]Track, error) {
	if err := g.ok(ctx); err != nil {
		return nil, err
	}
	return g.p.SearchTrack(ctx, q)
}

func (g *gated) GetTrack(ctx context.Context, id string) (*Track, error) {
	if err := g.ok(ctx); err != nil {
		return nil, err
	}
	return g.p.GetTrack(ctx, id)
}

func (g *gated) GetArtwork(ctx context.Context, id string) ([]ImageRef, error) {
	if err := g.ok(ctx); err != nil {
		return nil, err
	}
	return g.p.GetArtwork(ctx, id)
}

func (g *gated) GetArtistImage(ctx context.Context, id string) ([]ImageRef, error) {
	if err := g.ok(ctx); err != nil {
		return nil, err
	}
	return g.p.GetArtistImage(ctx, id)
}
