package enrich

import (
	"context"

	"younithm/internal/providers"
)

func albumKey(provider, id string) string { return provider + "/" + id }

// loadAlbums asks each catalogue that can (Spotify, Apple Music) for the albums
// the tracks sit on, through the snapshot cache, so the barcode of each album
// is known before anything is attached. These are calls to services we do not
// throttle like MusicBrainz; each album is asked about once per cache lifetime.
// An album that cannot be fetched is simply absent: its links are then made the
// old way, by id and title. Only a rate limit is returned, so the job can retry.
func (m *Manager) loadAlbums(ctx context.Context, tracks []*providers.Track) (map[string]providers.Album, error) {
	out := map[string]providers.Album{}
	for _, t := range tracks {
		p, ok := m.matcher.Providers.Get(t.Provider)
		if !ok {
			continue
		}
		al, ok := p.(providers.AlbumLookup)
		if !ok {
			continue
		}
		for _, r := range t.Releases {
			key := albumKey(t.Provider, r.ProviderID)
			if r.ProviderID == "" {
				continue
			}
			if _, done := out[key]; done {
				continue
			}
			a, err := m.getAlbum(ctx, al, t.Provider, r.ProviderID)
			if err != nil {
				if isRateLimited(err) {
					return nil, err
				}
				continue
			}
			out[key] = *a
		}
	}
	return out, nil
}
