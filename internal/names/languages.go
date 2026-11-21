package names

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

// LanguagesKey holds the preferred languages, comma separated, in display
// priority ("en,ja"). Absent or empty = just English. Names are fetched in
// each of them in batches (enrich/languages.go).
const LanguagesKey = "languages.preferred"

// DefaultLanguage is used when none are set.
const DefaultLanguage = "en"

// MaxLanguages bounds the list: each language costs a batch of requests.
const MaxLanguages = 5

var langRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// CleanLanguages lower-cases the language part, de-duplicates and validates
// tags, keeping the order. An empty list becomes just English.
func CleanLanguages(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		primary, rest, _ := strings.Cut(l, "-")
		l = strings.ToLower(primary)
		if rest != "" {
			l += "-" + rest
		}
		if !langRE.MatchString(l) {
			return nil, errors.New("\"" + l + "\" isn't a language code (try en, ja or zh-Hant)")
		}
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	if len(out) > MaxLanguages {
		return nil, errors.New("At most 5 languages")
	}
	if len(out) == 0 {
		out = []string{DefaultLanguage}
	}
	return out, nil
}

func subset(a, b []string) bool {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

// ReadLanguages reads a comma separated language setting; English when unset
// or unreadable.
func ReadLanguages(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) []string {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT value FROM admin_setting WHERE key=?`, key).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("names: reading %s: %v", key, err)
	}
	out, err := CleanLanguages(strings.Split(v, ","))
	if err != nil {
		return []string{DefaultLanguage}
	}
	return out
}

// MountLanguages registers GET/PUT /api/v1/admin/languages {"languages": ["en","ja"]},
// in display priority. onChange runs after a save that changed the list (the
// enrichment requeue).
func (s *Source) MountLanguages(r chi.Router, admin func(http.Handler) http.Handler, onChange func()) {
	r.With(admin).Get("/api/v1/admin/languages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"languages": ReadLanguages(r.Context(), s.read, LanguagesKey)})
	})
	r.With(admin).Put("/api/v1/admin/languages", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Languages []string `json:"languages"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "send {\"languages\": [\"en\"]}"})
			return
		}
		langs, err := CleanLanguages(in.Languages)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		old := ReadLanguages(r.Context(), s.read, LanguagesKey)
		if _, err := s.write.ExecContext(r.Context(), `INSERT INTO admin_setting (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
			LanguagesKey, strings.Join(langs, ",")); err != nil {
			log.Printf("names: saving languages: %v", err)
			writeJSON(w, 500, map[string]string{"error": "internal error"})
			return
		}
		s.Forget() // the display order of languages changed
		// Only a new language needs names fetched; a reorder does not.
		if onChange != nil && !subset(langs, old) {
			onChange()
		}
		writeJSON(w, 200, map[string]any{"languages": langs})
	})
}
