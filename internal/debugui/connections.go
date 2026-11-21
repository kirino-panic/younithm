package debugui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/providers"
	"younithm/internal/secrets"
)

// field is one value a provider needs.
type field struct {
	Name      string `json:"name"` // secrets.Store key
	Label     string `json:"label"`
	Multiline bool   `json:"multiline,omitempty"`
	// Reveal fields are not secret (a contact address), so their value is
	// shown back. Everything else is write-only, like the real admin API.
	Reveal bool   `json:"reveal,omitempty"`
	Help   string `json:"help,omitempty"`
}

type providerDef struct {
	ID     string
	Label  string
	Fields []field
	Note   string
}

var providerDefs = []providerDef{
	{ID: providers.MusicBrainz, Label: "MusicBrainz", Fields: []field{
		{Name: "musicbrainz.contact", Label: "Contact (email or URL)", Reveal: true,
			Help: "Goes in the User-Agent; MusicBrainz blocks clients without one. MUSICBRAINZ_CONTACT is used when nothing is saved here."},
	}, Note: "No account or key needed. Limited to one request per second."},
	{ID: providers.Spotify, Label: "Spotify", Fields: []field{
		{Name: "spotify.client_id", Label: "Client ID"},
		{Name: "spotify.client_secret", Label: "Client secret"},
	}, Note: "Client-credentials flow (catalog only). Create an app at developer.spotify.com."},
	{ID: providers.AppleMusic, Label: "Apple Music", Fields: []field{
		{Name: "apple.team_id", Label: "Team ID"},
		{Name: "apple.key_id", Label: "Key ID"},
		{Name: "apple.private_key", Label: "Private key (.p8 contents)", Multiline: true},
	}, Note: "Developer token signed with your MusicKit .p8 key. The keyless iTunes Search API is an opt-in stand-in (YOUNITHM_ENABLE_ITUNES)."},
	{ID: providers.ITunes, Label: "iTunes Search (fallback)", Note: "Keyless fallback for Apple Music, off by default (YOUNITHM_ENABLE_ITUNES=1). When on, it is active only while no Apple private key is saved, and it sends artist/title text to Apple. Nothing to enter."},
}

func fieldKnown(name string) bool {
	for _, p := range providerDefs {
		for _, f := range p.Fields {
			if f.Name == name {
				return true
			}
		}
	}
	return false
}

type fieldState struct {
	field
	Set       bool       `json:"set"`
	Hint      string     `json:"hint,omitempty"`
	Value     string     `json:"value,omitempty"` // Reveal fields only
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type providerState struct {
	ID         string       `json:"id"`
	Label      string       `json:"label"`
	Note       string       `json:"note,omitempty"`
	Fields     []fieldState `json:"fields"`
	Configured bool         `json:"configured"` // every field has a value (or the fallback applies)
	Source     string       `json:"source,omitempty"`
	InRegistry bool         `json:"in_registry"`
}

func (d Deps) connections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := []providerState{}
	for _, p := range providerDefs {
		ps := providerState{ID: p.ID, Label: p.Label, Note: p.Note, Fields: []fieldState{}}
		_, ps.InRegistry = d.Registry.Get(p.ID)
		all := len(p.Fields) > 0
		for _, f := range p.Fields {
			m, err := d.Secrets.Meta(ctx, f.Name)
			if err != nil {
				serverErr(w, "reading "+f.Name, err)
				return
			}
			fs := fieldState{field: f, Set: m.Set}
			if m.Set {
				fs.Hint = m.Hint
				if !m.UpdatedAt.IsZero() {
					t := m.UpdatedAt
					fs.UpdatedAt = &t
				}
				if f.Reveal {
					if v, err := d.Secrets.Get(ctx, f.Name); err == nil {
						fs.Value = v
					}
				}
			}
			all = all && m.Set
			ps.Fields = append(ps.Fields, fs)
		}
		ps.Configured = all
		if all {
			ps.Source = "saved here"
		}
		switch p.ID {
		case providers.MusicBrainz:
			if !all && d.MusicBrainzEnvContact != "" {
				ps.Configured, ps.Source = true, "MUSICBRAINZ_CONTACT env"
			}
			if d.MusicBrainz != nil {
				ps.InRegistry = ps.Configured
			}
		case providers.ITunes:
			ok, _ := d.Secrets.Has(ctx, "apple.private_key")
			switch {
			case !d.ITunesEnabled:
				ps.Configured, ps.Source = false, "off (set YOUNITHM_ENABLE_ITUNES=1 to turn on)"
			case ok:
				ps.Configured, ps.Source = false, "inactive: Apple key saved"
			default:
				ps.Configured, ps.Source = true, "active while no Apple key is saved"
			}
		}
		out = append(out, ps)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

const maxValueLen = 8 << 10

func (d Deps) setConnection(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !fieldKnown(name) {
		fail(w, http.StatusNotFound, "unknown field")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxValueLen+1024)
	var in struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	v := strings.TrimSpace(in.Value)
	if v == "" || len(v) > maxValueLen {
		fail(w, http.StatusBadRequest, "value is empty or too long")
		return
	}
	if err := d.Secrets.Set(r.Context(), name, v); err != nil {
		serverErr(w, "saving", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": name})
}

func (d Deps) deleteConnection(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !fieldKnown(name) {
		fail(w, http.StatusNotFound, "unknown field")
		return
	}
	if err := d.Secrets.Delete(r.Context(), name); err != nil && !errors.Is(err, secrets.ErrNotSet) {
		serverErr(w, "deleting", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testConnection asks the provider one real question and reports what came
// back, so a bad key or contact shows up here instead of deep in a matching run.
func (d Deps) testConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "provider")
	p, ok := d.Registry.Get(id)
	if !ok {
		fail(w, http.StatusNotFound, "provider is not registered")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	q := providers.TrackQuery{Artist: "Daft Punk", Title: "Get Lucky", Limit: 3}
	start := time.Now()
	tracks, err := p.SearchTrack(ctx, q)
	res := map[string]any{"query": q, "ms": time.Since(start).Milliseconds(), "ok": err == nil}
	if err != nil {
		kind := "error"
		var rl *providers.RateLimitedError
		switch {
		case errors.Is(err, providers.ErrNotConfigured):
			kind = "not_configured"
		case errors.As(err, &rl):
			kind = "rate_limited"
		}
		res["error"], res["error_kind"] = err.Error(), kind
	} else {
		res["results"] = tracks
	}
	writeJSON(w, http.StatusOK, res)
}
