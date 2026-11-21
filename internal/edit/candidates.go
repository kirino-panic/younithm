package edit

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"

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
  trim(COALESCE((SELECT group_concat(name, ', ') FROM (SELECT COALESCE(NULLIF(a.display_name, ''), a.name) AS name FROM release_artist ra
     JOIN artist a ON a.id = ra.artist_id WHERE ra.release_id = x.id ORDER BY ra.position)), '') || COALESCE(' · ' || substr(x.release_date, 1, 4), ''), ' ·'),
  (SELECT COUNT(*) FROM scrobble s WHERE s.release_id = x.id),
  ` + artwork.ReleaseImage("x") + `, x.id
FROM release x`,
	KindRecording: `SELECT x.id, COALESCE(NULLIF(x.display_name, ''), x.title),
  trim(COALESCE((SELECT group_concat(name, ', ') FROM (SELECT COALESCE(NULLIF(a.display_name, ''), a.name) AS name FROM recording_artist ra
     JOIN artist a ON a.id = ra.artist_id WHERE ra.recording_id = x.id ORDER BY ra.position)), '') || COALESCE(' · ' || (SELECT rel.title FROM recording_release rr
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

// MergeSearch finds entities of a kind, other than id, whose stored name,
// display name, catalogue name or a spelling it was scrobbled as contains q.
// Exact and prefix matches come first, then the most played.
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
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(lq) + "%"
	col := "x." + nameCol[kind]
	query := strings.Replace(base, "\nFROM ", ", '' AS reason\nFROM ", 1) + `
WHERE x.id <> ?4 AND (lower(` + col + `) LIKE ?1 ESCAPE '\' OR lower(COALESCE(x.display_name, '')) LIKE ?1 ESCAPE '\'
  OR EXISTS (SELECT 1 FROM entity_name n WHERE n.entity_type = '` + kind + `' AND n.entity_id = x.id AND lower(n.name) LIKE ?1 ESCAPE '\')
  OR EXISTS (SELECT 1 FROM alias al WHERE al.kind = '` + kind + `' AND al.entity_id = x.id AND lower(replace(al.raw_string, char(31), ' ')) LIKE ?1 ESCAPE '\'))
ORDER BY lower(` + col + `) = ?2 DESC, lower(COALESCE(x.display_name, '')) = ?2 DESC, lower(` + col + `) LIKE ?3 ESCAPE '\' DESC, 4 DESC
LIMIT ?5`
	prefix := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(lq) + "%"
	rows, err := db.QueryContext(ctx, query, like, lq, prefix, id, limit)
	if err != nil {
		return nil, err
	}
	return scanCandidates(rows, kind)
}

// MergeSuggestions lists entities that may be the same as id: ones holding the
// same catalogue id, then ones that go by one of its names (stored, display,
// catalogue or scrobbled spelling), then tracks whose title differs only by a
// suffix that says where the track appeared (see baseTitle). Same-named tracks
// and albums must also share an artist, so a common title like "Intro" does not
// flood the list.
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
		sharesArtist = `EXISTS (SELECT 1 FROM recording_artist a1 JOIN recording_artist a2 ON a2.artist_id = a1.artist_id
  WHERE a1.recording_id = ?1 AND a2.recording_id = x.id)`
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
	sameLink := `EXISTS (SELECT 1 FROM external_link l1 JOIN external_link l2 ON l2.provider = l1.provider AND l2.provider_id = l1.provider_id
  WHERE l1.entity_type = '` + kind + `' AND l1.entity_id = ?1 AND l1.status IN ('confirmed', 'auto')
    AND l2.entity_type = '` + kind + `' AND l2.entity_id = x.id AND l2.status IN ('confirmed', 'auto'))`
	reason := `CASE WHEN ` + sameLink + ` THEN 'Same catalogue entry' WHEN ` + sameName + ` THEN 'Same name' ELSE 'Similar name' END AS reason`
	query := strings.Replace(base, "\nFROM ", ", "+reason+"\nFROM ", 1) + `
WHERE x.id <> ?1 AND (` + sameLink + ` OR ((` + sameName + ` OR ` + similar + `) AND ` + sharesArtist + `))
ORDER BY reason = 'Same catalogue entry' DESC, reason = 'Same name' DESC, 4 DESC
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
