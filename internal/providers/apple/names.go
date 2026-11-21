package apple

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"younithm/internal/providers"
)

// Names in a language. Apple words its catalogue per storefront, and the l
// parameter only picks among the languages that storefront supports (the US
// store has no Japanese; the JP store has Japanese and English). So a language
// is asked for in the configured storefront when it supports it, and otherwise
// in a storefront that does, preferring one whose own default it is.

var _ providers.LocalizedNames = (*Client)(nil)

// Per-request id limits (Apple answers "Too Many Ids" above them).
var batchSizes = map[string]int{providers.KindArtist: 50, providers.KindAlbum: 100, providers.KindRecording: 300}

var paths = map[string]string{providers.KindArtist: "/artists", providers.KindAlbum: "/albums", providers.KindRecording: "/songs"}

const storefrontsTTL = 24 * time.Hour

func (c *Client) BatchSize(kind string) int { return batchSizes[kind] }

type storefront struct {
	ID        string
	Default   string
	Supported []string
}

// storefronts lists every storefront with its languages, cached for a day.
func (c *Client) storefronts(ctx context.Context) ([]storefront, error) {
	c.mu.Lock()
	if c.fronts != nil && c.cfg.Now().Sub(c.frontsAt) < storefrontsTTL {
		f := c.fronts
		c.mu.Unlock()
		return f, nil
	}
	c.mu.Unlock()
	var all []storefront
	next := "/storefronts?limit=200"
	for next != "" && len(all) < 1000 {
		var page struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					Default   string   `json:"defaultLanguageTag"`
					Supported []string `json:"supportedLanguageTags"`
				} `json:"attributes"`
			} `json:"data"`
			Next string `json:"next"`
		}
		if err := c.get(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, d := range page.Data {
			all = append(all, storefront{d.ID, d.Attributes.Default, d.Attributes.Supported})
		}
		next = strings.TrimPrefix(page.Next, "/v1")
	}
	c.mu.Lock()
	c.fronts, c.frontsAt = all, c.cfg.Now()
	c.mu.Unlock()
	return all, nil
}

// tagScore says how well a storefront's tag serves the wanted one: 2 for the
// same tag, 1 when the wanted tag is a prefix of it ("en" → "en-US",
// "zh-Hant" → "zh-Hant-TW"), 0 otherwise.
func tagScore(want, have string) int {
	w, h := strings.ToLower(want), strings.ToLower(have)
	switch {
	case w == h:
		return 2
	case strings.HasPrefix(h, w+"-"):
		return 1
	}
	return 0
}

// pickStorefront returns the storefront and l value to ask for lang.
func pickStorefront(fronts []storefront, home, lang string) (string, string, bool) {
	best := func(sf storefront) (string, int) {
		tag, score := "", 0
		for _, t := range sf.Supported {
			if s := tagScore(lang, t); s > score {
				tag, score = t, s
			}
		}
		return tag, score
	}
	for _, sf := range fronts {
		if sf.ID == home {
			if tag, s := best(sf); s > 0 {
				return sf.ID, tag, true
			}
		}
	}
	// A storefront whose own default is the language, then any that offers it.
	for _, wantDefault := range []bool{true, false} {
		for _, sf := range fronts {
			tag, s := best(sf)
			if s == 0 || (wantDefault && tagScore(lang, sf.Default) == 0) {
				continue
			}
			return sf.ID, tag, true
		}
	}
	return "", "", false
}

// NamesIn returns the names of up to BatchSize(kind) ids in lang. The answer is
// authoritative: it comes from a storefront that supports the language.
func (c *Client) NamesIn(ctx context.Context, kind string, ids []string, lang string) (map[string]string, bool, error) {
	path, ok := paths[kind]
	if !ok {
		return nil, false, providers.ErrUnsupported
	}
	var clean []string
	for _, id := range ids {
		if digits(id) {
			clean = append(clean, id)
		}
	}
	if max := batchSizes[kind]; len(clean) > max {
		clean = clean[:max]
	}
	out := map[string]string{}
	if len(clean) == 0 {
		return out, true, nil
	}
	fronts, err := c.storefronts(ctx)
	if err != nil {
		return nil, false, err
	}
	sf, tag, ok := pickStorefront(fronts, c.cfg.Storefront, lang)
	if !ok {
		return nil, false, providers.ErrUnsupported
	}
	v := url.Values{"ids": {strings.Join(clean, ",")}, "l": {tag}}
	var res struct{ Data []resource }
	if err := c.get(ctx, "/catalog/"+url.PathEscape(sf)+path+"?"+v.Encode(), &res); err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return out, true, nil
		}
		return nil, false, err
	}
	for _, r := range res.Data {
		if n := strings.TrimSpace(r.Attributes.Name); n != "" {
			out[r.ID] = n
		}
	}
	return out, true, nil
}
