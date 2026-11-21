package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// DB is the surface the resolver needs; *sql.Tx and *sql.DB satisfy it.
type DB interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// Report counts what resolving did. Used by the backfill preview.
type Report struct {
	ArtistsCreated    int `json:"artists_created"`
	ArtistsMatched    int `json:"artists_matched"`
	ArtistsUnverified int `json:"artists_unverified"`
	RecordingsCreated int `json:"recordings_created"`
	RecordingsMatched int `json:"recordings_matched"`
	ReleasesCreated   int `json:"releases_created"`
	ReleasesMatched   int `json:"releases_matched"`
	Candidates        int `json:"candidates"`
	LinksAdded        int `json:"links_added"`
}

// Add accumulates o into r.
func (r *Report) Add(o Report) {
	r.ArtistsCreated += o.ArtistsCreated
	r.ArtistsMatched += o.ArtistsMatched
	r.ArtistsUnverified += o.ArtistsUnverified
	r.RecordingsCreated += o.RecordingsCreated
	r.RecordingsMatched += o.RecordingsMatched
	r.ReleasesCreated += o.ReleasesCreated
	r.ReleasesMatched += o.ReleasesMatched
	r.Candidates += o.Candidates
	r.LinksAdded += o.LinksAdded
}

// Resolver finds or creates the entities for a listen. Rep accumulates counts
// across calls. Run inside a transaction.
type Resolver struct {
	Rep Report
	// Memo, when non-nil, remembers the result of each distinct listen so a
	// repeat play in the same run (an import) resolves without touching the
	// database. Only valid inside one transaction run with no merges between
	// calls; NewMemoResolver sets it up.
	Memo map[[16]byte][2]int64

	created []int64 // artists created during the current Resolve call
}

const memoMax = 20_000

// NewMemoResolver returns a Resolver that memoises repeated listens.
func NewMemoResolver() *Resolver { return &Resolver{Memo: map[[16]byte][2]int64{}} }

const (
	kArtist    = "artist"
	kRecording = "recording"
	kRelease   = "release"
)

// strong providers: two different ids of the same provider are two entities.
func strong(p string) bool { return p == MusicBrainz || p == Spotify }

type idRef struct{ prov, id, method string }

// Resolve returns the recording and release (0 = none) for the listen.
func (r *Resolver) Resolve(ctx context.Context, q DB, in Input) (recID, relID int64, err error) {
	r.created = r.created[:0]
	recID, relID, err = r.resolve(ctx, q, in)
	if err == nil {
		err = r.dropUnused(ctx, q)
	}
	return recID, relID, err
}

// dropUnused deletes artists this call created that ended up credited on
// nothing: a listen whose recording was found through an alias or an id never
// needed the artist its credit string would have created.
func (r *Resolver) dropUnused(ctx context.Context, q DB) error {
	for _, id := range r.created {
		var verified int
		err := q.QueryRowContext(ctx, `SELECT verified FROM artist WHERE id=?
			AND NOT EXISTS (SELECT 1 FROM recording_artist WHERE artist_id=artist.id)
			AND NOT EXISTS (SELECT 1 FROM release_artist WHERE artist_id=artist.id)`, id).Scan(&verified)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		for _, stmt := range []string{
			`DELETE FROM external_link WHERE entity_type='artist' AND entity_id=?`,
			`DELETE FROM alias WHERE kind='artist' AND entity_id=?`,
			`DELETE FROM merge_candidate WHERE kind='artist' AND (entity_id=? OR other_id=?)`,
			`DELETE FROM artist WHERE id=?`,
		} {
			args := []any{id}
			if strings.Contains(stmt, "other_id") {
				args = []any{id, id}
			}
			if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
				return err
			}
		}
		r.Rep.ArtistsCreated--
		if verified == 0 {
			r.Rep.ArtistsUnverified--
		}
	}
	return nil
}

// UnknownArtist is the credit given to a listen that names no artist, so that
// every recording still has an artist.
const UnknownArtist = "Unknown Artist"

func (r *Resolver) resolve(ctx context.Context, q DB, in Input) (recID, relID int64, err error) {
	if Norm(in.Title) == "" {
		return 0, 0, nil
	}
	if len(in.Credits) == 0 {
		in.Credits = []Credit{{Name: UnknownArtist}}
	}
	if r.Memo != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%#v", in)))
		var key [16]byte
		copy(key[:], sum[:])
		if v, ok := r.Memo[key]; ok {
			return v[0], v[1], nil
		}
		defer func() {
			if err == nil {
				if len(r.Memo) >= memoMax { // bounded: an import must stay streaming
					r.Memo = map[[16]byte][2]int64{}
				}
				r.Memo[key] = [2]int64{recID, relID}
			}
		}()
	}
	aids := make([]int64, len(in.Credits))
	for i, c := range in.Credits {
		cand := ""
		if i < len(in.CandArtistMBIDs) {
			cand = in.CandArtistMBIDs[i]
		}
		if aids[i], err = r.resolveArtist(ctx, q, c, cand); err != nil {
			return 0, 0, err
		}
	}
	if recID, err = r.resolveRecording(ctx, q, in, aids); err != nil {
		return 0, 0, err
	}
	relID, err = r.resolveRelease(ctx, q, in, recID, aids)
	return recID, relID, err
}

// ---- artists ----

func (r *Resolver) resolveArtist(ctx context.Context, q DB, c Credit, candMBID string) (int64, error) {
	var ids []idRef
	if c.MBID != "" {
		ids = append(ids, idRef{MusicBrainz, c.MBID, "submitted_mbid"})
	}
	if c.SpotifyID != "" {
		ids = append(ids, idRef{Spotify, c.SpotifyID, "submitted_id"})
	}
	if c.AppleID != "" {
		ids = append(ids, idRef{Apple, c.AppleID, "submitted_id"})
	}
	found := map[string]int64{}
	for _, x := range ids {
		id, err := entityByLink(ctx, q, kArtist, x.prov, x.id)
		if err != nil {
			return 0, err
		}
		if id != 0 && x.prov == Apple { // weak key: the name must agree too
			if ok, err := nameIs(ctx, q, id, c.Name); err != nil || !ok {
				if err != nil {
					return 0, err
				}
				continue
			}
		}
		if id != 0 {
			found[x.prov] = id
		}
	}
	var x int64
	for _, p := range []string{MusicBrainz, Spotify, Apple} {
		if found[p] != 0 {
			x = found[p]
			break
		}
	}
	for _, other := range found {
		if x != 0 && other != x {
			if err := r.candidate(ctx, q, kArtist, x, other, "ids point at different artists"); err != nil {
				return 0, err
			}
		}
	}
	if x == 0 {
		var created bool
		var err error
		if x, _, created, err = r.artistByName(ctx, q, c, ids); err != nil {
			return 0, err
		}
		if created { // ambiguous name: a new unverified row, already flagged
			return x, r.mappingCandidate(ctx, q, kArtist, x, candMBID)
		}
	}
	if x != 0 {
		conflict, err := hasConflict(ctx, q, kArtist, x, ids)
		if err != nil {
			return 0, err
		}
		if conflict {
			// Different ids of the same provider are different artists
			// whatever their names: no merge candidate either.
			return r.createArtist(ctx, q, c, ids)
		}
		r.Rep.ArtistsMatched++
		n, err := r.attach(ctx, q, kArtist, x, ids)
		if err != nil {
			return 0, err
		}
		if n > 0 {
			if _, err := q.ExecContext(ctx, `UPDATE artist SET verified=1 WHERE id=? AND verified=0`, x); err != nil {
				return 0, err
			}
		}
		if err := r.mappingCandidate(ctx, q, kArtist, x, candMBID); err != nil {
			return 0, err
		}
		return x, nil
	}
	id, err := r.createArtist(ctx, q, c, ids)
	if err != nil {
		return 0, err
	}
	return id, r.mappingCandidate(ctx, q, kArtist, id, candMBID)
}

// artistByName applies the single-match rule. When two or more artists share
// the name it creates a new unverified artist, flags it against each, and
// reports created=true.
func (r *Resolver) artistByName(ctx context.Context, q DB, c Credit, ids []idRef) (id int64, viaName, created bool, err error) {
	if Norm(c.Name) == "" {
		return 0, false, false, nil
	}
	var a int64
	err = q.QueryRowContext(ctx, `SELECT entity_id FROM alias WHERE kind='artist' AND raw_string=? AND
		EXISTS (SELECT 1 FROM artist WHERE id=entity_id)`, Norm(c.Name)).Scan(&a)
	if err == nil {
		return a, true, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM artist WHERE lower(trim(name)) = ? ORDER BY id`, Norm(c.Name))
	if err != nil {
		return 0, false, false, err
	}
	var cands []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, false, false, err
		}
		cands = append(cands, v)
	}
	rows.Close()
	switch len(cands) {
	case 0:
		return 0, false, false, nil
	case 1:
		return cands[0], true, false, nil
	}
	nid, err := r.createArtist(ctx, q, c, ids)
	if err != nil {
		return 0, false, false, err
	}
	for _, o := range cands {
		if err := r.candidate(ctx, q, kArtist, o, nid, "same name"); err != nil {
			return 0, false, false, err
		}
	}
	return nid, true, true, nil
}

func (r *Resolver) createArtist(ctx context.Context, q DB, c Credit, ids []idRef) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO artist (name, verified) VALUES (?,0)`, c.Name)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	r.created = append(r.created, id)
	r.Rep.ArtistsCreated++
	n, err := r.attach(ctx, q, kArtist, id, ids)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if _, err := q.ExecContext(ctx, `UPDATE artist SET verified=1 WHERE id=?`, id); err != nil {
			return 0, err
		}
	} else {
		r.Rep.ArtistsUnverified++
	}
	return id, nil
}

// ---- recordings ----

func (r *Resolver) resolveRecording(ctx context.Context, q DB, in Input, aids []int64) (int64, error) {
	ids := nonEmpty([]idRef{
		{MusicBrainz, in.RecordingMBID, "submitted_mbid"},
		{Spotify, in.SpotifyTrackID, "submitted_id"},
		{Apple, in.AppleSongID, "submitted_id"},
		{ISRCProv, in.ISRC, "isrc"},
	})
	rec := in.HintRecordingID
	byStrongID := false // found by a MusicBrainz or Spotify id, whatever the title
	if rec == 0 {
		for _, x := range ids {
			id, err := entityByLink(ctx, q, kRecording, x.prov, x.id)
			if err != nil {
				return 0, err
			}
			if id == 0 {
				continue
			}
			if x.prov == Apple || x.prov == ISRCProv { // weak: the title must agree
				if ok, err := titleIs(ctx, q, id, in.Title); err != nil || !ok {
					if err != nil {
						return 0, err
					}
					continue
				}
			}
			rec, byStrongID = id, strong(x.prov)
			break
		}
	}
	if rec == 0 {
		var err error
		if rec, err = recordingByAlias(ctx, q, in); err != nil {
			return 0, err
		}
	}
	if rec == 0 && len(aids) > 0 && aids[0] != 0 {
		err := q.QueryRowContext(ctx, `SELECT r.id FROM recording r JOIN recording_artist ra ON ra.recording_id=r.id
			WHERE ra.artist_id=? AND lower(trim(r.title))=? ORDER BY r.id LIMIT 1`, aids[0], Norm(in.Title)).Scan(&rec)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	if rec != 0 {
		conflict, err := hasConflict(ctx, q, kRecording, rec, ids)
		if err != nil {
			return 0, err
		}
		if conflict {
			rec, byStrongID = 0, false
		}
	}
	if rec == 0 {
		var dur any
		if in.DurationMs > 0 {
			dur = in.DurationMs
		}
		res, err := q.ExecContext(ctx, `INSERT INTO recording (title, duration_ms) VALUES (?,?)`, in.Title, dur)
		if err != nil {
			return 0, err
		}
		rec, _ = res.LastInsertId()
		r.Rep.RecordingsCreated++
	} else {
		r.Rep.RecordingsMatched++
		if in.DurationMs > 0 {
			if _, err := q.ExecContext(ctx, `UPDATE recording SET duration_ms=? WHERE id=? AND duration_ms IS NULL`, in.DurationMs, rec); err != nil {
				return 0, err
			}
		}
	}
	if err := r.credits(ctx, q, "recording_artist", "recording_id", rec, aids, in.Credits); err != nil {
		return 0, err
	}
	if _, err := r.attach(ctx, q, kRecording, rec, ids); err != nil {
		return 0, err
	}
	if byStrongID {
		if err := learnSpelling(ctx, q, rec, in); err != nil {
			return 0, err
		}
	}
	return rec, r.mappingCandidate(ctx, q, kRecording, rec, in.CandRecordingMBID)
}

// credits writes the artist rows of a recording or release. For an existing
// entity it only adds missing credits when its current artists are a subset
// of the new ones; it never rewrites a differing set (that is the split rule's
// job, with a confident provider match).
func (r *Resolver) credits(ctx context.Context, q DB, table, col string, id int64, aids []int64, cs []Credit) error {
	if len(aids) == 0 {
		return nil
	}
	have := map[int64]bool{}
	rows, err := q.QueryContext(ctx, `SELECT artist_id FROM `+table+` WHERE `+col+`=?`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a int64
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		have[a] = true
	}
	rows.Close()
	want := map[int64]bool{}
	for _, a := range aids {
		want[a] = true
	}
	for a := range have {
		if !want[a] {
			return nil
		}
	}
	for i, a := range aids {
		jp := any(nil)
		if i < len(cs) && cs[i].JoinPhrase != "" {
			jp = cs[i].JoinPhrase
		}
		var err error
		if table == "recording_artist" {
			_, err = q.ExecContext(ctx, `INSERT INTO recording_artist (recording_id, artist_id, role, position, join_phrase)
				VALUES (?,?, 'primary', ?, ?) ON CONFLICT(recording_id, artist_id, role) DO UPDATE SET
				join_phrase = COALESCE(recording_artist.join_phrase, excluded.join_phrase)`, id, a, i, jp)
		} else {
			_, err = q.ExecContext(ctx, `INSERT INTO release_artist (release_id, artist_id, position, join_phrase)
				VALUES (?,?,?,?) ON CONFLICT(release_id, artist_id) DO UPDATE SET
				join_phrase = COALESCE(release_artist.join_phrase, excluded.join_phrase)`, id, a, i, jp)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- releases ----

// resolveRelease finds or creates the album a listen was on. Album artists come
// only from album-artist data (in.AlbumArtists), never from the track's
// credits, so a compilation does not split into one album per track artist.
// A listen that names an album but no album artist attaches to the one existing
// album with that title (see releaseByTitle).
func (r *Resolver) resolveRelease(ctx context.Context, q DB, in Input, recID int64, aids []int64) (int64, error) {
	ids := nonEmpty([]idRef{
		{MusicBrainz, in.ReleaseMBID, "submitted_mbid"},
		{Spotify, in.SpotifyAlbumID, "submitted_id"},
		{Apple, in.AppleAlbumID, "submitted_id"},
	})
	title := Norm(in.Album)
	if title == "" && len(ids) == 0 && in.HintReleaseID == 0 {
		return 0, nil
	}
	var relAids []int64
	for _, c := range in.AlbumArtists {
		id, err := r.resolveArtist(ctx, q, c, "")
		if err != nil {
			return 0, err
		}
		relAids = append(relAids, id)
	}
	rel := in.HintReleaseID
	if rel == 0 {
		for _, x := range ids {
			id, err := entityByLink(ctx, q, kRelease, x.prov, x.id)
			if err != nil {
				return 0, err
			}
			if id == 0 {
				continue
			}
			if x.prov == Apple && title != "" { // weak: the title must agree
				if ok, err := releaseTitleIs(ctx, q, id, in.Album); err != nil || !ok {
					if err != nil {
						return 0, err
					}
					continue
				}
			}
			rel = id
			break
		}
	}
	var dupes []int64
	if rel == 0 && title != "" {
		var err error
		if rel, dupes, err = releaseByTitle(ctx, q, in.Album, relAids); err != nil {
			return 0, err
		}
	}
	if rel != 0 {
		conflict, err := hasConflict(ctx, q, kRelease, rel, ids)
		if err != nil {
			return 0, err
		}
		if conflict {
			rel = 0
		}
	}
	if rel == 0 {
		if title == "" {
			return 0, nil // ids only, no title to create a release with
		}
		res, err := q.ExecContext(ctx, `INSERT INTO release (title) VALUES (?)`, in.Album)
		if err != nil {
			return 0, err
		}
		rel, _ = res.LastInsertId()
		r.Rep.ReleasesCreated++
		for _, d := range dupes {
			if err := r.candidate(ctx, q, kRelease, d, rel, "same title, album artist unknown"); err != nil {
				return 0, err
			}
		}
	} else {
		r.Rep.ReleasesMatched++
	}
	if in.ReleaseGroupMBID != "" {
		if _, err := q.ExecContext(ctx, `UPDATE release SET release_group_mbid=? WHERE id=? AND release_group_mbid IS NULL`, in.ReleaseGroupMBID, rel); err != nil {
			return 0, err
		}
	}
	if err := r.credits(ctx, q, "release_artist", "release_id", rel, relAids, in.AlbumArtists); err != nil {
		return 0, err
	}
	if _, err := r.attach(ctx, q, kRelease, rel, ids); err != nil {
		return 0, err
	}
	if recID != 0 {
		// The first album seen for a recording becomes its primary release.
		if _, err := q.ExecContext(ctx,
			`INSERT OR IGNORE INTO recording_release (recording_id, release_id, is_primary)
			 VALUES (?,?, NOT EXISTS (SELECT 1 FROM recording_release WHERE recording_id=?))`, recID, rel, recID); err != nil {
			return 0, err
		}
	}
	return rel, nil
}

// releaseByTitle finds the album for a listen with no usable id, by title.
//
// With album artists known: the album with that title and one of those
// artists. Failing that, the only album with that title when it has no album
// artist yet (it is filled in). Otherwise a new album, flagged against any
// unattributed album of the same title.
//
// With no album artist: an alias, else the only album with that title, else
// the oldest album with that title that has no album artist, else (several
// albums of different artists share the title) id 0 plus those albums, so the
// caller creates one more and flags them rather than guess.
func releaseByTitle(ctx context.Context, q DB, album string, relAids []int64) (id int64, dupes []int64, err error) {
	if a, err := entityByAlias(ctx, q, kRelease, ReleaseAliasKey(album)); err != nil {
		return 0, nil, err
	} else if a != 0 {
		ok, err := releaseCompatible(ctx, q, a, relAids)
		if err != nil {
			return 0, nil, err
		}
		if ok {
			return a, nil, nil
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT id, NOT EXISTS (SELECT 1 FROM release_artist WHERE release_id=release.id)
		FROM release WHERE lower(trim(title)) = ? ORDER BY id`, Norm(album))
	if err != nil {
		return 0, nil, err
	}
	var cands, bare []int64
	for rows.Next() {
		var c int64
		var noArtist bool
		if err := rows.Scan(&c, &noArtist); err != nil {
			rows.Close()
			return 0, nil, err
		}
		cands = append(cands, c)
		if noArtist {
			bare = append(bare, c)
		}
	}
	rows.Close()
	if len(relAids) > 0 {
		for _, c := range cands {
			for _, a := range relAids {
				var n int
				if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_artist WHERE release_id=? AND artist_id=?`, c, a).Scan(&n); err != nil {
					return 0, nil, err
				}
				if n > 0 {
					return c, nil, nil
				}
			}
		}
		if len(cands) == 1 && len(bare) == 1 {
			return bare[0], nil, nil
		}
		return 0, bare, nil
	}
	switch {
	case len(cands) == 0:
		return 0, nil, nil
	case len(cands) == 1:
		return cands[0], nil, nil
	case len(bare) > 0:
		return bare[0], nil, nil
	}
	return 0, cands, nil
}

// releaseCompatible reports whether a release found by alias may take a listen
// whose album artists are relAids: unknown artists on either side are fine,
// otherwise they must share one.
func releaseCompatible(ctx context.Context, q DB, rel int64, relAids []int64) (bool, error) {
	if len(relAids) == 0 {
		return true, nil
	}
	var total int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_artist WHERE release_id=?`, rel).Scan(&total); err != nil {
		return false, err
	}
	if total == 0 {
		return true, nil
	}
	for _, a := range relAids {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_artist WHERE release_id=? AND artist_id=?`, rel, a).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// recordingByAlias looks the listen's artist and title up in the recording
// aliases (names learned from edits, merges and provider lookups).
func recordingByAlias(ctx context.Context, q DB, in Input) (int64, error) {
	keys := []string{}
	if Norm(in.Artist) != "" {
		keys = append(keys, RecordingAliasKey(in.Artist, in.Title))
	}
	var names []string
	for _, c := range in.Credits {
		if Norm(c.Name) != "" {
			names = append(names, c.Name)
			keys = append(keys, RecordingAliasKey(c.Name, in.Title))
		}
	}
	if len(names) > 1 {
		keys = append(keys, RecordingAliasKey(strings.Join(names, ", "), in.Title))
	}
	for _, k := range keys {
		if id, err := entityByAlias(ctx, q, kRecording, k); err != nil || id != 0 {
			return id, err
		}
	}
	return 0, nil
}

// learnSpelling saves the listen's artist and title as an alias of rec, which
// a MusicBrainz or Spotify id has just proved it to be, so a later listen
// written the same way but carrying no id still finds it. The track's own
// title needs no alias.
func learnSpelling(ctx context.Context, q DB, rec int64, in Input) error {
	if ok, err := titleIs(ctx, q, rec, in.Title); err != nil || ok {
		return err
	}
	artist := in.Artist
	if Norm(artist) == "" {
		var names []string
		for _, c := range in.Credits {
			if Norm(c.Name) != "" {
				names = append(names, c.Name)
			}
		}
		artist = strings.Join(names, ", ")
	}
	if Norm(artist) == "" {
		return nil
	}
	_, err := AddAlias(ctx, q, kRecording, rec, RecordingAliasKey(artist, in.Title))
	return err
}

// ---- helpers ----

func nonEmpty(in []idRef) []idRef {
	var out []idRef
	for _, x := range in {
		if x.id != "" {
			out = append(out, x)
		}
	}
	return out
}

// entityByLink finds the entity holding a non-candidate link.
func entityByLink(ctx context.Context, q DB, kind, prov, id string) (int64, error) {
	var e int64
	err := q.QueryRowContext(ctx, `SELECT entity_id FROM external_link
		WHERE entity_type=? AND provider=? AND provider_id=? AND status <> 'candidate' ORDER BY id LIMIT 1`,
		kind, prov, id).Scan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return e, err
}

// hasConflict reports whether entity already holds a different id of a strong
// provider than one of ids (two ids of the same provider = two entities).
func hasConflict(ctx context.Context, q DB, kind string, entity int64, ids []idRef) (bool, error) {
	for _, x := range ids {
		if !strong(x.prov) {
			continue
		}
		// An entity that holds the id itself is not in conflict with it, whatever
		// else it holds: a recording may hold a MusicBrainz track id the listen
		// named beside the recording id that track resolved to.
		var n int
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_link
			WHERE entity_type=? AND entity_id=? AND provider=? AND provider_id<>? AND status <> 'candidate'
			AND NOT EXISTS (SELECT 1 FROM external_link WHERE entity_type=? AND entity_id=? AND provider=? AND provider_id=? AND status <> 'candidate')`,
			kind, entity, x.prov, x.id, kind, entity, x.prov, x.id).Scan(&n)
		if err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// attach adds each id the entity does not hold and no other entity owns. It
// returns how many links were added.
func (r *Resolver) attach(ctx context.Context, q DB, kind string, entity int64, ids []idRef) (int, error) {
	added := 0
	for _, x := range ids {
		owner, err := entityByLink(ctx, q, kind, x.prov, x.id)
		if err != nil {
			return added, err
		}
		if owner != 0 {
			if owner == entity {
				added++ // already corroborated
			}
			continue
		}
		ok, err := r.addLink(ctx, q, kind, entity, x.prov, x.id, x.method, "auto")
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}

func (r *Resolver) addLink(ctx context.Context, q DB, kind string, entity int64, prov, id, method, status string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM link_rejection
		WHERE entity_type=? AND entity_id=? AND provider=? AND provider_id=?`, kind, entity, prov, id).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO external_link
		(entity_type, entity_id, provider, provider_id, match_method, confidence, status)
		VALUES (?,?,?,?,?,?,?)`, kind, entity, prov, id, method, 1.0, status)
	if err != nil {
		return false, err
	}
	a, _ := res.RowsAffected()
	if a > 0 {
		r.Rep.LinksAdded++
	}
	return a > 0, nil
}

// mappingCandidate stores an uncorroborated ListenBrainz mbid_mapping id.
func (r *Resolver) mappingCandidate(ctx context.Context, q DB, kind string, entity int64, mbid string) error {
	if mbid == "" {
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_link
		WHERE entity_type=? AND entity_id=? AND provider=? AND status <> 'candidate'`, kind, entity, MusicBrainz).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	ok, err := r.addLink(ctx, q, kind, entity, MusicBrainz, mbid, "mapping", "candidate")
	if ok {
		r.Rep.Candidates++
	}
	return err
}

func (r *Resolver) candidate(ctx context.Context, q DB, kind string, a, b int64, reason string) error {
	if a == b {
		return nil
	}
	if a > b {
		a, b = b, a
	}
	res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO merge_candidate (kind, entity_id, other_id, reason) VALUES (?,?,?,?)`, kind, a, b, reason)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		r.Rep.Candidates++
	}
	return nil
}

func nameIs(ctx context.Context, q DB, artist int64, name string) (bool, error) {
	var n string
	if err := q.QueryRowContext(ctx, `SELECT name FROM artist WHERE id=?`, artist).Scan(&n); err != nil {
		return false, err
	}
	return Norm(n) == Norm(name), nil
}

func titleIs(ctx context.Context, q DB, rec int64, title string) (bool, error) {
	var t string
	if err := q.QueryRowContext(ctx, `SELECT title FROM recording WHERE id=?`, rec).Scan(&t); err != nil {
		return false, err
	}
	return Norm(t) == Norm(title), nil
}

func releaseTitleIs(ctx context.Context, q DB, rel int64, title string) (bool, error) {
	var t string
	if err := q.QueryRowContext(ctx, `SELECT title FROM release WHERE id=?`, rel).Scan(&t); err != nil {
		return false, err
	}
	return Norm(t) == Norm(title), nil
}
