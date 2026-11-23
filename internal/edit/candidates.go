package edit

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"younithm/internal/names"

	"younithm/internal/artwork"
)

// Candidate is an entity offered as a merge target, with what tells look-alikes
// apart: its credit line or album, plays and image.
type Candidate struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Sub    string `json:"sub,omitempty"`
	Plays  int64  `json:"plays"`
	Image  string `json:"image,omitempty"`
	Reason string `json:"reason,omitempty"` // suggestions only: why it may be the same
}

// candidateSQL is, per kind, the columns every candidate query selects
// (id, shown name, sub line, plays, image path, image owner id) and the table
// alias they use (x).
var candidateSQL = map[string]string{
	KindArtist: `SELECT x.id, COALESCE(NULLIF(x.display_name, ''), x.name),
  (SELECT COUNT(*) || CASE COUNT(*) WHEN 1 THEN ' track' ELSE ' tracks' END FROM recording_artist ra WHERE ra.artist_id = x.id),
  (SELECT COUNT(*) FROM scrobble s JOIN recording_artist ra ON ra.recording_id = s.recording_id WHERE ra.artist_id = x.id),
  ` + artwork.ArtistImage("x") + `, x.id
FROM artist x`,
	KindRelease: `SELECT x.id, COALESCE(NULLIF(x.display_name, ''), x.title),
  trim(COALESCE(` + names.LineSQL(names.Release, "x.id") + `, '') || COALESCE(' · ' || substr(x.release_date, 1, 4), ''), ' ·'),
  (SELECT COUNT(*) FROM scrobble s WHERE s.release_id = x.id),
  ` + artwork.ReleaseImage("x") + `, x.id
FROM release x`,
	KindRecording: `SELECT x.id, COALESCE(NULLIF(x.display_name, ''), x.title),
  trim(COALESCE(` + names.LineSQL(names.Recording, "x.id") + `, '') || COALESCE(' · ' || (SELECT rel.title FROM recording_release rr
     JOIN release rel ON rel.id = rr.release_id WHERE rr.recording_id = x.id ORDER BY rr.is_primary DESC, rel.id LIMIT 1), ''), ' ·'),
  (SELECT COUNT(*) FROM scrobble s WHERE s.recording_id = x.id),
  (SELECT ` + artwork.ReleaseImage("rel") + ` FROM recording_release rr JOIN release rel ON rel.id = rr.release_id
     WHERE rr.recording_id = x.id AND COALESCE(` + artwork.ReleaseImage("rel") + `, '') <> '' ORDER BY rr.is_primary DESC, rel.id LIMIT 1),
  (SELECT rel.id FROM recording_release rr JOIN release rel ON rel.id = rr.release_id
     WHERE rr.recording_id = x.id AND COALESCE(` + artwork.ReleaseImage("rel") + `, '') <> '' ORDER BY rr.is_primary DESC, rel.id LIMIT 1)
FROM recording x`,
}

// nameCol is the stored-name column of each kind.
var nameCol = map[string]string{KindArtist: "name", KindRelease: "title", KindRecording: "title"}

func scanCandidates(rows *sql.Rows, kind string) ([]Candidate, error) {
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		var sub, img sql.NullString
		var imgID sql.NullInt64
		var reason string
		if err := rows.Scan(&c.ID, &c.Name, &sub, &c.Plays, &img, &imgID, &reason); err != nil {
			return nil, err
		}
		c.Sub, c.Reason = sub.String, reason
		if img.String != "" && imgID.Valid {
			ak := artwork.KindRelease
			if kind == KindArtist {
				ak = artwork.KindArtist
			}
			c.Image = artwork.URL(ak, imgID.Int64, img.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// looseChars are dropped by loose matching, so "Monday, Monday" finds
// "Monday Monday" and "dont" finds "Don't".
const looseChars = `,.'’‘"“”!?:;…`

// looseSQL wraps a lowercased SQL text expression so it drops looseChars.
func looseSQL(expr string) string {
	for _, r := range looseChars {
		expr = "replace(" + expr + ", '" + strings.ReplaceAll(string(r), "'", "''") + "', '')"
	}
	return expr
}

// loose is looseSQL in Go, for the query side.
func loose(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(looseChars, r) {
			return -1
		}
		return r
	}, s)
}

// MergeSearch finds entities of a kind, other than id, whose stored name,
// display name, catalogue name or a spelling it was scrobbled as contains q,
// ignoring the punctuation in looseChars. Tracks and albums sharing an artist
// with id come first, then exact and prefix matches, then the most played.
func MergeSearch(ctx context.Context, db Querier, kind string, id int64, q string, limit int) ([]Candidate, error) {
	base, ok := candidateSQL[kind]
	if !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return []Candidate{}, nil
	}
	lq := strings.ToLower(q)
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	like := "%" + esc.Replace(lq) + "%"
	looseLike := "%" + esc.Replace(strings.TrimSpace(loose(lq))) + "%"
	if looseLike == "%%" { // the query was only punctuation
		looseLike = like
	}
	col := "x." + nameCol[kind]
	match := func(expr string) string {
		return `(` + expr + ` LIKE ?1 ESCAPE '\' OR ` + looseSQL(expr) + ` LIKE ?6 ESCAPE '\')`
	}
	// Tracks and albums sharing an artist with id come first: a merge is
	// nearly always between two entries of the same artist.
	sameArtist := ""
	switch kind {
	case KindRecording:
		sameArtist = `EXISTS (SELECT 1 FROM recording_artist a1 JOIN recording_artist a2 ON a2.artist_id = a1.artist_id
  WHERE a1.recording_id = ?4 AND a2.recording_id = x.id) DESC, `
	case KindRelease:
		sameArtist = `EXISTS (SELECT 1 FROM release_artist a1 JOIN release_artist a2 ON a2.artist_id = a1.artist_id
  WHERE a1.release_id = ?4 AND a2.release_id = x.id) DESC, `
	}
	query := strings.Replace(base, "\nFROM ", ", '' AS reason\nFROM ", 1) + `
WHERE x.id <> ?4 AND (` + match("lower("+col+")") + ` OR ` + match("lower(COALESCE(x.display_name, ''))") + `
  OR EXISTS (SELECT 1 FROM entity_name n WHERE n.entity_type = '` + kind + `' AND n.entity_id = x.id AND ` + match("lower(n.name)") + `)
  OR EXISTS (SELECT 1 FROM alias al WHERE al.kind = '` + kind + `' AND al.entity_id = x.id AND ` + match("lower(replace(al.raw_string, char(31), ' '))") + `)` + searchNames(kind, match) + `)
ORDER BY ` + sameArtist + `lower(` + col + `) = ?2 DESC, lower(COALESCE(x.display_name, '')) = ?2 DESC, lower(` + col + `) LIKE ?3 ESCAPE '\' DESC, 4 DESC
LIMIT ?5`
	prefix := esc.Replace(lq) + "%"
	rows, err := db.QueryContext(ctx, query, like, lq, prefix, id, limit, looseLike)
	if err != nil {
		return nil, err
	}
	return scanCandidates(rows, kind)
}

// MergeSuggestions lists entities that may be the same as id: ones holding the
// same catalogue id, then ones that go by one of its names (stored, display,
// catalogue or scrobbled spelling), then tracks whose title differs only by a
// suffix that says where the track appeared (see baseTitle). Albums in the same
// MusicBrainz release group (editions: mono and stereo, standard and deluxe)
// come right after catalogue matches. Same-named tracks must share an artist or
// an album, and same-named albums an artist, so a common title like "Intro"
// does not flood the list.
func MergeSuggestions(ctx context.Context, db Querier, kind string, id int64, limit int) ([]Candidate, error) {
	base, ok := candidateSQL[kind]
	if !ok {
		return nil, invalid("bad_kind", "kind must be artist, recording or release")
	}
	names, err := entityNames(ctx, db, kind, id)
	if err != nil {
		return nil, err
	}
	js, _ := json.Marshal(names)
	col := "x." + nameCol[kind]
	anyName := func(cond string) string { // cond tests the placeholder {v} against one of x's names
		return `(` + strings.ReplaceAll(cond, "{v}", "lower("+col+")") + ` OR ` + strings.ReplaceAll(cond, "{v}", "lower(COALESCE(x.display_name, ''))") + `
  OR EXISTS (SELECT 1 FROM entity_name n WHERE n.entity_type = '` + kind + `' AND n.entity_id = x.id AND ` + strings.ReplaceAll(cond, "{v}", "lower(n.name)") + `))`
	}
	sameName := anyName(`{v} IN (SELECT value FROM json_each(?2))`)
	sharesArtist := "1"
	similar, bases := "0", []string{}
	switch kind {
	case KindRecording:
		// Sharing an album counts too: a track scrobbled under a credit string
		// that became its own artist still lands on the album by title.
		sharesArtist = `(EXISTS (SELECT 1 FROM recording_artist a1 JOIN recording_artist a2 ON a2.artist_id = a1.artist_id
  WHERE a1.recording_id = ?1 AND a2.recording_id = x.id)
  OR EXISTS (SELECT 1 FROM recording_release r1 JOIN recording_release r2 ON r2.release_id = r1.release_id
  WHERE r1.recording_id = ?1 AND r2.recording_id = x.id))`
		bases = baseTitles(names)
		if len(bases) > 0 {
			// A rough net (same base, or base plus any suffix); kept rows are
			// checked against baseTitle below.
			similar = anyName(`({v} IN (SELECT value FROM json_each(?4)) OR EXISTS (SELECT 1 FROM json_each(?5) p WHERE {v} LIKE p.value ESCAPE '\'))`)
		}
	case KindRelease:
		sharesArtist = `EXISTS (SELECT 1 FROM release_artist a1 JOIN release_artist a2 ON a2.artist_id = a1.artist_id
  WHERE a1.release_id = ?1 AND a2.release_id = x.id)`
	}
	var patterns []string
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for _, b := range bases {
		e := esc.Replace(b)
		patterns = append(patterns, e+" (%", e+" [%", e+" - %")
	}
	bjs, _ := json.Marshal(bases)
	pjs, _ := json.Marshal(patterns)
	sameGroup := "0"
	if kind == KindRelease {
		sameGroup = `EXISTS (SELECT 1 FROM release g WHERE g.id = ?1 AND COALESCE(g.release_group_mbid, '') <> '' AND g.release_group_mbid = x.release_group_mbid)`
	}
	sameLink := `EXISTS (SELECT 1 FROM external_link l1 JOIN external_link l2 ON l2.provider = l1.provider AND l2.provider_id = l1.provider_id
  WHERE l1.entity_type = '` + kind + `' AND l1.entity_id = ?1 AND l1.status IN ('confirmed', 'auto')
    AND l2.entity_type = '` + kind + `' AND l2.entity_id = x.id AND l2.status IN ('confirmed', 'auto'))`
	reason := `CASE WHEN ` + sameLink + ` THEN 'Same catalogue entry' WHEN ` + sameGroup + ` THEN 'Same release group' WHEN ` + sameName + ` THEN 'Same name' ELSE 'Similar name' END AS reason`
	query := strings.Replace(base, "\nFROM ", ", "+reason+"\nFROM ", 1) + `
WHERE x.id <> ?1 AND (` + sameLink + ` OR ` + sameGroup + ` OR ((` + sameName + ` OR ` + similar + `) AND ` + sharesArtist + `))
ORDER BY reason = 'Same catalogue entry' DESC, reason = 'Same release group' DESC, reason = 'Same name' DESC, 4 DESC
LIMIT ?3`
	rows, err := db.QueryContext(ctx, query, id, string(js), limit*2, string(bjs), string(pjs))
	if err != nil {
		return nil, err
	}
	got, err := scanCandidates(rows, kind)
	if err != nil {
		return nil, err
	}
	out := got[:0]
	for _, c := range got {
		if c.Reason == "Similar name" {
			ok, err := sharesBase(ctx, db, kind, c.ID, bases)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		if len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

// placementSuffix matches the text of a title suffix that only says where a
// track appeared, not that it is a different recording: "bonus track",
// "album version", "remastered 2011". Version tags such as live, remix,
// acoustic or demo are deliberately left out.
var placementSuffix = regexp.MustCompile(`^(?:bonus(?: track)?|(?:album|single|lp) version|(?:\d{4} )?remaster(?:ed)?(?: \d{4})?(?: version)?)$`)

// titleSuffix splits a lowercased title into its head and a trailing
// "(...)", "[...]" or " - ..." suffix.
var titleSuffix = regexp.MustCompile(`^(.+)\s*(?:\(([^()]*)\)|\[([^\[\]]*)\]|\s-\s+(.+))$`)

// baseTitle drops placement suffixes from a lowercased title, so
// "song (bonus track)" and "song - 2011 remaster" both become "song".
func baseTitle(s string) string {
	s = strings.TrimSpace(s)
	for {
		m := titleSuffix.FindStringSubmatch(s)
		if m == nil {
			return s
		}
		inner := strings.TrimSpace(m[2] + m[3] + m[4])
		if !placementSuffix.MatchString(inner) {
			return s
		}
		s = strings.TrimSpace(m[1])
	}
}

// baseTitles is the distinct non-empty base titles of names.
func baseTitles(names []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range names {
		if b := baseTitle(n); b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// sharesBase reports whether one of the entity's names has a base title in bases.
func sharesBase(ctx context.Context, db Querier, kind string, id int64, bases []string) (bool, error) {
	names, err := entityNames(ctx, db, kind, id)
	if err != nil {
		return false, err
	}
	for _, b := range baseTitles(names) {
		for _, want := range bases {
			if b == want {
				return true, nil
			}
		}
	}
	return false, nil
}

// entityNames is every name the entity goes by, lowercased: stored, display,
// searchNames matches an entity's search-only names.
func searchNames(kind string, match func(string) string) string {
	return `
  OR EXISTS (SELECT 1 FROM search_name sn WHERE sn.kind = '` + kind + `' AND sn.entity_id = x.id AND ` + match("replace(sn.name, char(31), ' ')") + `)`
}

// catalogue names and (for artists and albums) the spellings it was scrobbled as.
func entityNames(ctx context.Context, db Querier, kind string, id int64) ([]string, error) {
	t := kindTable[kind]
	q := `SELECT lower(` + nameCol[kind] + `) FROM ` + t + ` WHERE id = ?1
UNION SELECT lower(display_name) FROM ` + t + ` WHERE id = ?1 AND COALESCE(display_name, '') <> ''
UNION SELECT lower(name) FROM entity_name WHERE entity_type = ?2 AND entity_id = ?1 AND name <> ''`
	if kind != KindRecording { // recording alias keys carry the artist too
		q += `
UNION SELECT lower(raw_string) FROM alias WHERE kind = ?2 AND entity_id = ?1`
	}
	rows, err := db.QueryContext(ctx, q, id, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
