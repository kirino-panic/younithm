// Package names picks which wording is shown for artists, albums and tracks.
// The stored names never change. A priority order over Spotify, Apple Music,
// MusicBrainz and the scrobbled (stored) name decides: each catalogue's own
// name is read from the cached provider answers through the entity's accepted
// link, and a source with no name for the entity is skipped.
package names

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// SettingKey holds the preference in admin_setting: a mode ("" = default),
// and for "custom" the order after a colon ("custom:apple,spotify,…").
const SettingKey = "display.naming"

// Scrobbled is the stored name: what listens sent, unless a person renamed it.
// A display name set by hand (display_name) shows over every source.
const Scrobbled = "scrobbled"

// Providers are the catalogues a name can come from.
var Providers = []string{"spotify", "apple", "musicbrainz"}

// DefaultOrder is the priority when nothing else is chosen.
var DefaultOrder = []string{"spotify", "apple", "musicbrainz", Scrobbled}

// Modes a person can pick.
const (
	ModeDefault = "default"
	ModeCustom  = "custom"
)

// Pref is the chosen mode and the priority it resolves to.
type Pref struct {
	Mode  string   `json:"mode"`
	Order []string `json:"order"`
}

// resolve turns a mode (and a custom order) into a full priority order. A
// single source goes first, the rest follow in the default order.
func resolve(mode string, custom []string) (Pref, bool) {
	switch mode {
	case "", ModeDefault:
		return Pref{ModeDefault, append([]string(nil), DefaultOrder...)}, true
	case ModeCustom:
		if len(custom) != len(DefaultOrder) {
			return Pref{}, false
		}
		seen := map[string]bool{}
		for _, c := range custom {
			if seen[c] || !contains(DefaultOrder, c) {
				return Pref{}, false
			}
			seen[c] = true
		}
		return Pref{ModeCustom, append([]string(nil), custom...)}, true
	}
	if !contains(DefaultOrder, mode) {
		return Pref{}, false
	}
	order := []string{mode}
	for _, d := range DefaultOrder {
		if d != mode {
			order = append(order, d)
		}
	}
	return Pref{mode, order}, true
}

func parse(v string) Pref {
	mode, rest, _ := strings.Cut(v, ":")
	var custom []string
	if rest != "" {
		custom = strings.Split(rest, ",")
	}
	if p, ok := resolve(mode, custom); ok {
		return p
	}
	p, _ := resolve("", nil)
	return p
}

func (p Pref) encode() string {
	if p.Mode == ModeCustom {
		return p.Mode + ":" + strings.Join(p.Order, ",")
	}
	return p.Mode
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Source resolves display names. read is the read pool; write saves the setting.
type Source struct {
	read, write *sql.DB

	mu    sync.Mutex
	cache map[string]cached
	pref  *cachedPref
}

type cached struct {
	name string
	at   time.Time
}

type cachedPref struct {
	v  Pref
	at time.Time
}

const ttl = 5 * time.Minute

func New(read, write *sql.DB) *Source {
	return &Source{read: read, write: write, cache: map[string]cached{}}
}

// Pref returns the chosen mode and priority order.
func (s *Source) Pref(ctx context.Context) Pref {
	if s == nil {
		return Pref{Scrobbled, []string{Scrobbled}}
	}
	s.mu.Lock()
	if s.pref != nil && time.Since(s.pref.at) < ttl {
		v := s.pref.v
		s.mu.Unlock()
		return v
	}
	s.mu.Unlock()
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM admin_setting WHERE key=?`, SettingKey).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("names: reading preference: %v", err)
	}
	p := parse(v)
	s.mu.Lock()
	s.pref = &cachedPref{p, time.Now()}
	s.mu.Unlock()
	return p
}

// Active reports whether anything can replace stored names: a catalogue's
// wording, or a display name set by hand.
func (s *Source) Active(ctx context.Context) bool {
	return s != nil && (s.Pref(ctx).Order[0] != Scrobbled || s.anyOverride(ctx))
}

func (s *Source) setPref(ctx context.Context, p Pref) error {
	_, err := s.write.ExecContext(ctx, `INSERT INTO admin_setting (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, SettingKey, p.encode())
	if err == nil {
		s.mu.Lock()
		s.pref = &cachedPref{p, time.Now()}
		s.cache = map[string]cached{}
		s.mu.Unlock()
	}
	return err
}

// Forget drops cached names, after a link changed.
func (s *Source) Forget() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cache = map[string]cached{}
	s.mu.Unlock()
}

// Kinds of entity.
const (
	Artist    = "artist"
	Recording = "recording"
	Release   = "release"
)

// Name returns provider's wording for entity (kind, id), or "" when unknown.
func (s *Source) Name(ctx context.Context, provider, kind string, id int64) string {
	if s == nil || provider == "" {
		return ""
	}
	var pid string
	err := s.read.QueryRowContext(ctx, `SELECT provider_id FROM external_link
WHERE entity_type=? AND entity_id=? AND provider=? AND status IN ('confirmed','auto')
ORDER BY status='confirmed' DESC, pinned DESC, confidence DESC, id LIMIT 1`, kind, id, provider).Scan(&pid)
	if err != nil {
		return ""
	}
	return s.byID(ctx, provider, kind, pid)
}

// byID reads the name a catalogue gives its own id, from the cached answers.
func (s *Source) byID(ctx context.Context, provider, kind, pid string) string {
	key := provider + "\x00" + kind + "\x00" + pid
	s.mu.Lock()
	if c, ok := s.cache[key]; ok && time.Since(c.at) < ttl {
		s.mu.Unlock()
		return c.name
	}
	s.mu.Unlock()
	var name sql.NullString
	var err error
	switch kind {
	case Recording:
		err = s.read.QueryRowContext(ctx, `SELECT json_extract(raw_json,'$.Title') FROM provider_snapshot
WHERE provider=? AND provider_id=? AND kind='recording' AND status='ok'`, provider, pid).Scan(&name)
	case Release:
		// The album's own answer (MusicBrainz caches a list of releases), else
		// the album as a track lists it.
		err = s.read.QueryRowContext(ctx, `SELECT CASE json_type(raw_json)
  WHEN 'array' THEN (SELECT json_extract(e.value,'$.Title') FROM json_each(raw_json) e WHERE json_extract(e.value,'$.ProviderID')=?2)
  ELSE json_extract(raw_json,'$.Title') END
FROM provider_snapshot WHERE provider=?1 AND provider_id=?2 AND kind='release' AND status='ok'`, provider, pid).Scan(&name)
		if err != nil || !name.Valid {
			err = s.read.QueryRowContext(ctx, `SELECT json_extract(r.value,'$.Title') FROM provider_snapshot s, json_each(s.raw_json,'$.Releases') r
WHERE s.provider=? AND s.kind='recording' AND s.status='ok' AND json_extract(r.value,'$.ProviderID')=? LIMIT 1`, provider, pid).Scan(&name)
		}
	case Artist:
		err = s.read.QueryRowContext(ctx, `SELECT json_extract(a.value,'$.Name') FROM provider_snapshot s, json_each(s.raw_json,'$.Artists') a
WHERE s.provider=? AND s.kind='recording' AND s.status='ok' AND json_extract(a.value,'$.ProviderID')=? LIMIT 1`, provider, pid).Scan(&name)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("names: %s %s %s: %v", provider, kind, pid, err)
	}
	v := strings.TrimSpace(name.String)
	s.mu.Lock()
	s.cache[key] = cached{v, time.Now()}
	s.mu.Unlock()
	return v
}

// Display returns the wording of the first source in the priority order that
// names the entity. Within a source, its name in the first preferred language
// it has one in (entity_name) comes first, then the name its cached answer
// gives. A display name a person set by hand always wins: it is what they
// chose to see.
func (s *Source) Display(ctx context.Context, kind string, id int64, stored string) string {
	if s == nil {
		return stored
	}
	if o := s.Override(ctx, kind, id); o != "" {
		return o
	}
	for _, src := range s.Pref(ctx).Order {
		if src == Scrobbled {
			return stored
		}
		if n := s.InLanguage(ctx, src, kind, id); n != "" {
			return n
		}
		if n := s.Name(ctx, src, kind, id); n != "" {
			return n
		}
	}
	return stored
}

// InLanguage returns provider's name for the entity in the first preferred
// language it has one in (entity_name), or "".
func (s *Source) InLanguage(ctx context.Context, provider, kind string, id int64) string {
	key := "lang\x00" + provider + "\x00" + kind + "\x00" + strconv.FormatInt(id, 10)
	s.mu.Lock()
	if c, ok := s.cache[key]; ok && time.Since(c.at) < ttl {
		s.mu.Unlock()
		return c.name
	}
	s.mu.Unlock()
	langs := s.Languages(ctx)
	rows, err := s.read.QueryContext(ctx, `SELECT lang, name FROM entity_name
WHERE entity_type = ? AND entity_id = ? AND provider = ? AND name <> ''`, kind, id, provider)
	if err != nil {
		log.Printf("names: languages %s %d: %v", kind, id, err)
		return ""
	}
	byLang := map[string]string{}
	for rows.Next() {
		var l, n string
		if rows.Scan(&l, &n) == nil {
			byLang[l] = n
		}
	}
	rows.Close()
	v := ""
	for _, l := range langs {
		if n := byLang[l]; n != "" {
			v = n
			break
		}
	}
	s.mu.Lock()
	s.cache[key] = cached{v, time.Now()}
	s.mu.Unlock()
	return v
}

// Languages is the preferred languages, cached like the names.
func (s *Source) Languages(ctx context.Context) []string {
	s.mu.Lock()
	if c, ok := s.cache["langs"]; ok && time.Since(c.at) < ttl {
		s.mu.Unlock()
		return strings.Split(c.name, ",")
	}
	s.mu.Unlock()
	l := ReadLanguages(ctx, s.read, LanguagesKey)
	s.mu.Lock()
	s.cache["langs"] = cached{strings.Join(l, ","), time.Now()}
	s.mu.Unlock()
	return l
}

// Names lists every name the entity goes by, in the naming order: each
// catalogue's names in the preferred languages, then its own answer, with the
// stored name at the Scrobbled slot (and last, if the order leaves it out).
// It may repeat; callers drop the duplicates.
func (s *Source) Names(ctx context.Context, kind string, id int64, stored string) []string {
	if s == nil {
		return []string{stored}
	}
	byProv := map[string]map[string]string{}
	rows, err := s.read.QueryContext(ctx, `SELECT provider, lang, name FROM entity_name
WHERE entity_type = ? AND entity_id = ? AND name <> ''`, kind, id)
	if err != nil {
		log.Printf("names: all names %s %d: %v", kind, id, err)
	} else {
		for rows.Next() {
			var p, l, n string
			if rows.Scan(&p, &l, &n) == nil {
				if byProv[p] == nil {
					byProv[p] = map[string]string{}
				}
				byProv[p][l] = n
			}
		}
		rows.Close()
	}
	langs := s.Languages(ctx)
	var out []string
	for _, src := range s.Pref(ctx).Order {
		if src == Scrobbled {
			out = append(out, stored)
			continue
		}
		for _, l := range langs {
			if n := byProv[src][l]; n != "" {
				out = append(out, n)
			}
		}
		if n := s.Name(ctx, src, kind, id); n != "" {
			out = append(out, n)
		}
	}
	return append(out, stored)
}

// Override is the display name a person set by hand for the entity, or "".
func (s *Source) Override(ctx context.Context, kind string, id int64) string {
	key := "override\x00" + kind + "\x00" + strconv.FormatInt(id, 10)
	if v, ok := s.cachedName(key); ok {
		return v
	}
	var v sql.NullString
	if t := tables[kind]; t != "" {
		err := s.read.QueryRowContext(ctx, `SELECT display_name FROM `+t+` WHERE id = ?`, id).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("names: override %s %d: %v", kind, id, err)
		}
	}
	s.store(key, strings.TrimSpace(v.String))
	return strings.TrimSpace(v.String)
}

// tables maps an entity kind to its table (never user input).
var tables = map[string]string{Artist: "artist", Recording: "recording", Release: "release"}

// anyOverride reports whether any entity has a display name set by hand.
func (s *Source) anyOverride(ctx context.Context) bool {
	const key = "override\x00any"
	if v, ok := s.cachedName(key); ok {
		return v != ""
	}
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM artist WHERE display_name IS NOT NULL)
  OR EXISTS (SELECT 1 FROM recording WHERE display_name IS NOT NULL)
  OR EXISTS (SELECT 1 FROM release WHERE display_name IS NOT NULL)`).Scan(&n)
	if err != nil {
		log.Printf("names: overrides: %v", err)
	}
	v := ""
	if n > 0 {
		v = "1"
	}
	s.store(key, v)
	return v != ""
}

func (s *Source) cachedName(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[key]; ok && time.Since(c.at) < ttl {
		return c.name, true
	}
	return "", false
}

func (s *Source) store(key, v string) {
	s.mu.Lock()
	s.cache[key] = cached{v, time.Now()}
	s.mu.Unlock()
}

// Credit is one artist of a credit line.
type Credit struct {
	ID           int64
	Name         string
	JoinPhrase   string
	CreditedName string // the name the artist is credited as here, "" = its own
}

// Line joins credits with their join phrases (", " when none). Each artist
// goes by the name it is credited as here (that is this release's own
// wording), else its name in the preferred wording.
func (s *Source) Line(ctx context.Context, cs []Credit) string {
	var b strings.Builder
	for i, c := range cs {
		name := c.CreditedName
		if name == "" {
			name = s.Display(ctx, Artist, c.ID, c.Name)
		}
		b.WriteString(name)
		if i < len(cs)-1 {
			if c.JoinPhrase != "" {
				b.WriteString(c.JoinPhrase)
			} else {
				b.WriteString(", ")
			}
		}
	}
	return b.String()
}

// Credits loads a recording's or release's credits in order.
func (s *Source) Credits(ctx context.Context, kind string, id int64) ([]Credit, error) {
	q := `SELECT a.id, a.name, COALESCE(ra.join_phrase,''), COALESCE(ra.credited_name,'') FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
WHERE ra.recording_id=? ORDER BY ra.position, a.id`
	if kind == Release {
		q = `SELECT a.id, a.name, COALESCE(ra.join_phrase,''), COALESCE(ra.credited_name,'') FROM release_artist ra JOIN artist a ON a.id=ra.artist_id
WHERE ra.release_id=? ORDER BY ra.position, a.id`
	}
	rows, err := s.read.QueryContext(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credit
	for rows.Next() {
		var c Credit
		if err := rows.Scan(&c.ID, &c.Name, &c.JoinPhrase, &c.CreditedName); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreditLine is Line over the stored credits; "" when there are none.
func (s *Source) CreditLine(ctx context.Context, kind string, id int64) string {
	cs, err := s.Credits(ctx, kind, id)
	if err != nil || len(cs) == 0 {
		return ""
	}
	return s.Line(ctx, cs)
}

// Variant is how one source names a recording.
type Variant struct {
	Source  string `json:"source"` // spotify, apple, musicbrainz, stored, submitted
	Title   string `json:"title"`
	Artists string `json:"artists,omitempty"`
}

// RecordingVariants lists every distinct name a recording goes by: each
// linked catalogue's title and credit, the stored one, and what listens sent.
func (s *Source) RecordingVariants(ctx context.Context, id int64, stored string) []Variant {
	seen := map[string]bool{}
	var out []Variant
	add := func(v Variant) {
		k := strings.ToLower(v.Title) + "\x00" + strings.ToLower(v.Artists)
		if v.Title == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, v)
	}
	add(Variant{Source: "stored", Title: stored, Artists: s.storedLine(ctx, id)})
	rows, err := s.read.QueryContext(ctx, `SELECT l.provider, s.raw_json FROM external_link l
JOIN provider_snapshot s ON s.provider=l.provider AND s.provider_id=l.provider_id AND s.kind='recording' AND s.status='ok'
WHERE l.entity_type='recording' AND l.entity_id=? AND l.status IN ('confirmed','auto')
ORDER BY l.status='confirmed' DESC, l.confidence DESC, l.id`, id)
	if err == nil {
		for rows.Next() {
			var p, raw string
			if rows.Scan(&p, &raw) != nil {
				continue
			}
			var t struct {
				Title   string
				Artists []struct{ Name, JoinPhrase string }
			}
			if json.Unmarshal([]byte(raw), &t) != nil {
				continue
			}
			var b strings.Builder
			for i, a := range t.Artists {
				b.WriteString(a.Name)
				if i < len(t.Artists)-1 {
					if a.JoinPhrase != "" {
						b.WriteString(a.JoinPhrase)
					} else {
						b.WriteString(", ")
					}
				}
			}
			add(Variant{Source: p, Title: t.Title, Artists: b.String()})
		}
		rows.Close()
	}
	rows, err = s.read.QueryContext(ctx, `SELECT raw_title, raw_artist FROM scrobble WHERE recording_id=?
GROUP BY raw_title, raw_artist ORDER BY COUNT(*) DESC LIMIT 10`, id)
	if err == nil {
		for rows.Next() {
			var t, a string
			if rows.Scan(&t, &a) == nil {
				add(Variant{Source: "submitted", Title: t, Artists: a})
			}
		}
		rows.Close()
	}
	return out
}

func (s *Source) storedLine(ctx context.Context, id int64) string {
	cs, err := s.Credits(ctx, Recording, id)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for i, c := range cs {
		b.WriteString(c.Name)
		if i < len(cs)-1 {
			if c.JoinPhrase != "" {
				b.WriteString(c.JoinPhrase)
			} else {
				b.WriteString(", ")
			}
		}
	}
	return b.String()
}

// Mount registers GET/PUT /api/v1/admin/naming {"mode", "order"}. mode is
// default, spotify, apple, musicbrainz, scrobbled or custom; order is read for
// custom only and must list every source once.
func (s *Source) Mount(r chi.Router, admin func(http.Handler) http.Handler) {
	r.With(admin).Get("/api/v1/admin/naming", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Pref(r.Context()))
	})
	r.With(admin).Put("/api/v1/admin/naming", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode  string   `json:"mode"`
			Order []string `json:"order"`
		}
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in)
		p, ok := resolve(in.Mode, in.Order)
		if err != nil || in.Mode == "" || !ok {
			writeJSON(w, 400, map[string]string{"error": "mode must be default, spotify, apple, musicbrainz, scrobbled or custom (with an order listing each source once)"})
			return
		}
		if err := s.setPref(r.Context(), p); err != nil {
			log.Printf("names: saving preference: %v", err)
			writeJSON(w, 500, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, 200, p)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
