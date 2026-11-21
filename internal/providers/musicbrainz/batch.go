package musicbrainz

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"younithm/internal/providers"
)

// Artist names by language, many artists at a time (one request, at 1 per
// second, names up to 100).

var _ providers.ArtistNamesLookup = (*Client)(nil)

const maxArtists = 100 // ids per artist search (one page of results)

func (c *Client) MaxArtists() int { return maxArtists }

// ArtistNames returns each artist's aliases that carry a language
// (GET /artist?query=arid:… OR arid:…). Aliases with no locale say nothing
// about a language, search hints are not names, and a legal name is not what
// the artist goes by (Sokudo! has "Aden Tirado", locale en, primary): all are
// left out.
func (c *Client) ArtistNames(ctx context.Context, ids []string) (map[string][]providers.LocalName, error) {
	out := map[string][]providers.LocalName{}
	var terms []string
	for _, id := range ids {
		if validID(id) {
			terms = append(terms, "arid:"+id)
		}
		if len(terms) == maxArtists {
			break
		}
	}
	if len(terms) == 0 {
		return out, nil
	}
	v := url.Values{"query": {strings.Join(terms, " OR ")}, "limit": {strconv.Itoa(maxArtists)}, "fmt": {"json"}}
	var res struct {
		Artists []struct {
			ID      string `json:"id"`
			Aliases []struct {
				Name    string `json:"name"`
				Locale  string `json:"locale"`
				Primary *bool  `json:"primary"`
				Type    string `json:"type"`
			} `json:"aliases"`
		} `json:"artists"`
	}
	if err := c.get(ctx, c.cfg.BaseURL+"/artist?"+v.Encode(), &res); err != nil {
		return nil, err
	}
	for _, a := range res.Artists {
		names := []providers.LocalName{}
		for _, al := range a.Aliases {
			if al.Locale == "" || strings.TrimSpace(al.Name) == "" || al.Type == "Search hint" || al.Type == "Legal name" {
				continue
			}
			names = append(names, providers.LocalName{
				Lang:    strings.ReplaceAll(al.Locale, "_", "-"),
				Name:    strings.TrimSpace(al.Name),
				Primary: al.Primary != nil && *al.Primary,
			})
		}
		out[a.ID] = names
	}
	return out, nil
}

// ---- Dynamic ----

var _ providers.ArtistNamesLookup = (*Dynamic)(nil)

func (d *Dynamic) MaxArtists() int { return maxArtists }

func (d *Dynamic) ArtistNames(ctx context.Context, ids []string) (map[string][]providers.LocalName, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.ArtistNames(ctx, ids)
}
