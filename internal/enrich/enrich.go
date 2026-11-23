// Package enrich is the enrichment job queue and the review queue
// (docs/providers.md). A sweep queues every recording that has no
// provider link; a single worker runs the matching ladder for each and writes
// external_link rows. Confident matches become 'auto' links; ambiguous ones
// become 'candidate' links, which are the review queue the admin resolves
// (see review.go). Rejected candidates are remembered and never re-proposed.
package enrich

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"younithm/internal/identity"
	"younithm/internal/providers"
	"younithm/internal/providers/match"
	"younithm/internal/providers/snapshot"
)

const tsLayout = "2006-01-02T15:04:05.000Z"

// Job statuses and outcomes (enrichment_job.status / outcome).
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"

	OutcomeLinked     = "linked"
	OutcomeCandidates = "candidates"
	OutcomeNoMatch    = "no_match"
)

type Options struct {
	Poll        time.Duration // how often to look for due jobs; default 15s
	SweepEvery  time.Duration // how often to queue unlinked recordings; default 10m
	Retry       time.Duration // re-queue unlinked recordings this long after the last try; default 7d
	MaxAttempts int           // errors before a job is marked failed; default 5
	Now         func() time.Time
	// Merge folds one entity into another (the editor's merge: logged and
	// undoable). Pairs marked ReasonBarcodeVariant are merged through it.
	// Nil leaves them flagged.
	Merge func(ctx context.Context, kind string, from, into int64) error
}

func (o Options) withDefaults() Options {
	if o.Poll <= 0 {
		o.Poll = 15 * time.Second
	}
	if o.SweepEvery <= 0 {
		o.SweepEvery = 10 * time.Minute
	}
	if o.Retry <= 0 {
		o.Retry = 7 * 24 * time.Hour
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Manager owns the queue. db must be the write pool.
type Manager struct {
	db      *sql.DB
	matcher *match.Matcher
	snaps   *snapshot.Store
	o       Options
	wake    chan struct{}
	sweep   atomic.Bool

	namesDue  atomic.Bool // a names pass should run when the queue is idle
	lastNames time.Time

	creditedDone  atomic.Bool // the credited-name backfill has nothing left
	creditedPause time.Time   // MusicBrainz failed: try the backfill again after this
}

func NewManager(db *sql.DB, matcher *match.Matcher, snaps *snapshot.Store, o Options) *Manager {
	return &Manager{db: db, matcher: matcher, snaps: snaps, o: o.withDefaults(), wake: make(chan struct{}, 1)}
}

// SweepNow makes Run queue new recordings now, for example after an import,
// instead of waiting for the next periodic sweep.
func (m *Manager) SweepNow() {
	m.sweep.Store(true)
	m.Wake()
}

// Wake makes Run look for due jobs now instead of at the next poll.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) stamp(t time.Time) string { return t.UTC().Format(tsLayout) }

// Run recovers interrupted jobs, then sweeps and works until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if _, err := m.db.ExecContext(ctx,
		`UPDATE enrichment_job SET status='queued' WHERE status='running'`); err != nil {
		log.Printf("enrich: recovering running jobs: %v", err)
	}
	var lastSweep time.Time
	t := time.NewTicker(m.o.Poll)
	defer t.Stop()
	for {
		if m.sweep.Swap(false) || m.o.Now().Sub(lastSweep) >= m.o.SweepEvery {
			m.sweepNow(ctx)
			lastSweep = m.o.Now()
		}
		n, err := m.ProcessDue(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("enrich: %v", err)
		}
		// Names are fetched in batches once matching has gone quiet, so a big
		// import is named in a few requests rather than one per track.
		if n == 0 && err == nil && (m.namesDue.Load() || m.o.Now().Sub(m.lastNames) >= m.o.SweepEvery) {
			m.namesDue.Store(false)
			m.lastNames = m.o.Now()
			m.autoMerge(ctx) // flags from before a rule existed, or whose proof arrived later
			if filled, err := m.fillTrackPositions(ctx); err != nil && ctx.Err() == nil {
				log.Printf("enrich: track numbers: %v", err)
			} else if filled > 0 {
				log.Printf("enrich: filled %d track numbers", filled)
			}
			if err := m.NamePass(ctx); err != nil && ctx.Err() == nil {
				log.Printf("enrich: names pass: %v", err)
			}
		}
		// Older albums get their credited names a few at a time while idle.
		if n == 0 && err == nil {
			if _, err := m.creditedNamesPass(ctx); err != nil && ctx.Err() == nil {
				log.Printf("enrich: credited-name backfill: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
		}
	}
}

// autoMerge merges the release pairs matching marked as the same album by
// barcode, folding the release without ids into the one holding the barcode.
func (m *Manager) autoMerge(ctx context.Context) {
	if m.o.Merge == nil {
		return
	}
	rows, err := m.db.QueryContext(ctx, `SELECT c.id, c.entity_id, c.other_id,
  EXISTS (SELECT 1 FROM external_link l WHERE l.entity_type='release' AND l.entity_id=c.entity_id)
FROM merge_candidate c WHERE c.kind='release' AND c.reason=?`, ReasonBarcodeVariant)
	if err != nil {
		log.Printf("enrich: auto merge: %v", err)
		return
	}
	type pair struct {
		id, a, b int64
		aHolds   bool
	}
	var ps []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.a, &p.b, &p.aHolds); err == nil {
			ps = append(ps, p)
		}
	}
	rows.Close()
	for _, p := range ps {
		into, from := p.a, p.b
		if !p.aHolds {
			into, from = p.b, p.a
		}
		if err := m.o.Merge(ctx, identity.KindRelease, from, into); err != nil {
			log.Printf("enrich: merging release %d into %d: %v", from, into, err)
			// Leave a plain flag for a person instead of retrying forever.
			_, _ = m.db.ExecContext(ctx, `UPDATE merge_candidate SET reason='same barcode as the catalogue''s album' WHERE id=?`, p.id)
			continue
		}
		// Flags on the release merged away are settled with it.
		_, _ = m.db.ExecContext(ctx, `DELETE FROM merge_candidate WHERE kind='release' AND (id=? OR entity_id=? OR other_id=?)`, p.id, from, from)
		log.Printf("enrich: merged release %d into %d (same album, same barcode)", from, into)
	}
	m.autoMergeRecordings(ctx)
}

// recordingISRCs are a recording's ISRCs: its own ISRC links and those its
// trusted catalogue answers list (?1 is the recording).
const recordingISRCs = `
  SELECT upper(l.provider_id) AS isrc FROM external_link l
  WHERE l.entity_type = 'recording' AND l.entity_id = ?1 AND l.provider = 'isrc' AND l.status IN ('confirmed', 'auto')
  UNION
  SELECT upper(i.value) FROM external_link l
  JOIN provider_snapshot s ON s.provider = l.provider AND s.provider_id = l.provider_id AND s.kind = 'recording' AND s.status = 'ok'
  JOIN json_each(s.raw_json, '$.ISRCs') i
  WHERE l.entity_type = 'recording' AND l.entity_id = ?1 AND ` + trustedTrackLink

// maxLengthGap is how far apart two copies of one recording may run: the
// same audio on two albums or catalogues differs by a second or two, while an
// edit or extended mix sharing the ISRC by mistake differs far more.
const maxLengthGap = 3000 // ms

// recordingLengths are the lengths a recording's trusted catalogue answers
// give (?1 is the recording).
const recordingLengths = `
  SELECT json_extract(s.raw_json, '$.DurationMs') AS ms FROM external_link l
  JOIN provider_snapshot s ON s.provider = l.provider AND s.provider_id = l.provider_id AND s.kind = 'recording' AND s.status = 'ok'
  WHERE l.entity_type = 'recording' AND l.entity_id = ?1 AND ` + trustedTrackLink + `
    AND COALESCE(json_extract(s.raw_json, '$.DurationMs'), 0) > 0`

// distinctMusicBrainz is true when the two recordings (?1, ?2) hold different
// MusicBrainz recordings and none in common: MusicBrainz says they differ.
const distinctMusicBrainz = `(
    EXISTS (SELECT 1 FROM external_link WHERE entity_type = 'recording' AND entity_id = ?1 AND provider = 'musicbrainz'
            AND status IN ('confirmed', 'auto') AND match_method <> 'mapping')
    AND EXISTS (SELECT 1 FROM external_link WHERE entity_type = 'recording' AND entity_id = ?2 AND provider = 'musicbrainz'
            AND status IN ('confirmed', 'auto') AND match_method <> 'mapping')
    AND NOT EXISTS (SELECT 1 FROM external_link x JOIN external_link y ON y.provider_id = x.provider_id
            WHERE x.entity_type = 'recording' AND x.entity_id = ?1 AND x.provider = 'musicbrainz' AND x.status IN ('confirmed', 'auto')
              AND y.entity_type = 'recording' AND y.entity_id = ?2 AND y.provider = 'musicbrainz' AND y.status IN ('confirmed', 'auto')))`

// sameByISRC proves a pair one song by ISRC and length: both recordings'
// trusted catalogue answers share an ISRC and every length they give is within
// maxLengthGap (?3) of the others.
var sameByISRC = `
  EXISTS (SELECT 1 FROM (` + recordingISRCs + `) a
          JOIN (` + strings.ReplaceAll(recordingISRCs, "?1", "?2") + `) b ON a.isrc = b.isrc)
  AND (SELECT MAX(ms) - MIN(ms) <= ?3 AND COUNT(DISTINCT src) = 2 FROM (
         SELECT ms, 1 AS src FROM (` + recordingLengths + `)
         UNION ALL SELECT ms, 2 FROM (` + strings.ReplaceAll(recordingLengths, "?1", "?2") + `)))`

// sameOnAlbum proves a pair flagged ReasonSameAlbumTrack: the two are still on
// one album of ours, and their own lengths, where both are known, are within
// maxLengthGap (?3).
const sameOnAlbum = `
  EXISTS (SELECT 1 FROM recording_release a JOIN recording_release b ON b.release_id = a.release_id
          WHERE a.recording_id = ?1 AND b.recording_id = ?2)
  AND NOT EXISTS (SELECT 1 FROM recording x, recording y WHERE x.id = ?1 AND y.id = ?2
          AND x.duration_ms > 0 AND y.duration_ms > 0 AND abs(x.duration_ms - y.duration_ms) > ?3)`

// autoMergeRecordings merges recordings flagged as one song, when ids prove
// it. A catalogue id found for one is held by the other ("same spotify id"),
// or an ISRC found for one is held by the other ("same ISRC"): both
// recordings' trusted catalogue answers must share an ISRC with lengths that
// agree. This is how two imports of one song end up apart (a Spotify listen
// and an Apple listen, each creating a recording by its own id, or one song on
// two albums). Or the tracklist of an album both are on names them as one
// track (ReasonSameAlbumTrack): they only need to be on that album still, with
// lengths that agree. Either way the two must not hold different MusicBrainz
// recordings, and a merge a person undid is not made again. The older
// recording is kept; the merge is undoable like any edit. Pairs without proof
// stay flagged for a person.
func (m *Manager) autoMergeRecordings(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT id, entity_id, other_id, reason FROM merge_candidate
WHERE kind = 'recording' AND (reason LIKE 'same % id' OR reason IN ('same ISRC', ?))`, ReasonSameAlbumTrack)
	if err != nil {
		log.Printf("enrich: auto merge recordings: %v", err)
		return
	}
	type pair struct {
		id, a, b int64
		reason   string
	}
	var ps []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.a, &p.b, &p.reason); err == nil {
			ps = append(ps, p)
		}
	}
	rows.Close()
	for _, p := range ps {
		proof, why, failed := sameByISRC, "same ISRC and length", "same recording by ISRC and length"
		if p.reason == ReasonSameAlbumTrack {
			proof, why, failed = sameOnAlbum, ReasonSameAlbumTrack, "one track on the album"
		}
		var proven bool
		err := m.db.QueryRowContext(ctx, `SELECT (`+proof+`) AND NOT `+distinctMusicBrainz, p.a, p.b, maxLengthGap).Scan(&proven)
		if err != nil {
			log.Printf("enrich: checking recordings %d and %d: %v", p.a, p.b, err)
			continue
		}
		if !proven {
			continue
		}
		into, from := p.a, p.b
		if from < into {
			into, from = from, into
		}
		if m.mergeUndone(ctx, identity.KindRecording, from, into) {
			continue
		}
		if err := m.o.Merge(ctx, identity.KindRecording, from, into); err != nil {
			log.Printf("enrich: merging recording %d into %d: %v", from, into, err)
			_, _ = m.db.ExecContext(ctx, `UPDATE merge_candidate SET reason = ? WHERE id = ?`, failed, p.id)
			continue
		}
		_, _ = m.db.ExecContext(ctx, `DELETE FROM merge_candidate WHERE kind = 'recording' AND (id = ? OR entity_id = ? OR other_id = ?)`, p.id, from, from)
		log.Printf("enrich: merged recording %d into %d (%s)", from, into, why)
	}
}

func (m *Manager) sweepNow(ctx context.Context) {
	if n, err := m.Sweep(ctx); err != nil {
		log.Printf("enrich: sweep: %v", err)
	} else if n > 0 {
		log.Printf("enrich: queued %d recordings", n)
	}
}

// Sweep queues every recording that has no job yet, most-played first, and
// revives jobs that finished without any provider link once the retry window
// has passed. Recordings that already carry links are queued too: catalog ids
// we only stored (a Spotify or Apple id with no ISRC) still need their lookup,
// and every recording gets its artists, album and name variants filled in.
// It returns how many jobs were created or revived.
func (m *Manager) Sweep(ctx context.Context) (int64, error) {
	now := m.o.Now()
	ns, cutoff := m.stamp(now), m.stamp(now.Add(-m.o.Retry))
	const unlinked = `NOT EXISTS (SELECT 1 FROM external_link l
	    WHERE l.entity_type='recording' AND l.entity_id=r.id AND l.match_method <> 'mapping')`
	res, err := m.db.ExecContext(ctx, `
INSERT OR IGNORE INTO enrichment_job (recording_id, status, priority, next_attempt_at, created_at, updated_at)
SELECT r.id, 'queued', (SELECT COUNT(*) FROM scrobble s WHERE s.recording_id=r.id), ?1, ?1, ?1
FROM recording r WHERE NOT EXISTS (SELECT 1 FROM enrichment_job j WHERE j.recording_id=r.id)`, ns)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = m.db.ExecContext(ctx, `
UPDATE enrichment_job SET status='queued', attempts=0, next_attempt_at=?1, updated_at=?1,
  priority=(SELECT COUNT(*) FROM scrobble s WHERE s.recording_id=enrichment_job.recording_id)
WHERE status IN ('done','failed') AND updated_at < ?2 AND EXISTS (
  SELECT 1 FROM recording r WHERE r.id=enrichment_job.recording_id AND `+unlinked+`)`, ns, cutoff)
	if err != nil {
		return n, err
	}
	n2, _ := res.RowsAffected()
	return n + n2, nil
}

// RequeueAll queues every recording again, for example after a provider was
// connected, so ids and names from the new provider are filled in. Running
// jobs are left alone. It returns how many jobs were queued.
func (m *Manager) RequeueAll(ctx context.Context) (int64, error) {
	ns := m.stamp(m.o.Now())
	if _, err := m.Sweep(ctx); err != nil {
		return 0, err
	}
	res, err := m.db.ExecContext(ctx, `
UPDATE enrichment_job SET status='queued', attempts=0, next_attempt_at=?1, last_error=NULL, updated_at=?1,
  priority=(SELECT COUNT(*) FROM scrobble s WHERE s.recording_id=enrichment_job.recording_id)
WHERE status <> 'running'`, ns)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Requeue forces a recording to be matched again now.
func (m *Manager) Requeue(ctx context.Context, recordingID int64) error {
	ns := m.stamp(m.o.Now())
	_, err := m.db.ExecContext(ctx, `
INSERT INTO enrichment_job (recording_id, status, next_attempt_at, created_at, updated_at)
VALUES (?1, 'queued', ?2, ?2, ?2)
ON CONFLICT(recording_id) DO UPDATE SET status='queued', attempts=0, next_attempt_at=?2,
  last_error=NULL, updated_at=?2 WHERE status <> 'running'`, recordingID, ns)
	return err
}

// RequeueRelease forces every recording on a release to be matched again now,
// for example after a person linked the album, so its tracklist can place them.
func (m *Manager) RequeueRelease(ctx context.Context, releaseID int64) error {
	ns := m.stamp(m.o.Now())
	_, err := m.db.ExecContext(ctx, `
INSERT INTO enrichment_job (recording_id, status, next_attempt_at, created_at, updated_at)
SELECT recording_id, 'queued', ?2, ?2, ?2 FROM recording_release WHERE release_id = ?1 AND true
ON CONFLICT(recording_id) DO UPDATE SET status='queued', attempts=0, next_attempt_at=?2,
  last_error=NULL, updated_at=?2 WHERE status <> 'running'`, releaseID, ns)
	return err
}

type job struct {
	id, recordingID int64
	attempts        int
}

// ProcessDue works every job that is due and returns how many it ran.
func (m *Manager) ProcessDue(ctx context.Context) (int, error) {
	n := 0
	for ctx.Err() == nil {
		// An import that finished mid-run joins the queue now, not after it drains.
		if m.sweep.Swap(false) {
			m.sweepNow(ctx)
		}
		j, err := m.claim(ctx)
		if err != nil {
			return n, err
		}
		if j == nil {
			return n, nil
		}
		m.handle(ctx, *j)
		m.autoMerge(ctx)
		n++
	}
	return n, ctx.Err()
}

func (m *Manager) claim(ctx context.Context) (*job, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var j job
	err = tx.QueryRowContext(ctx, `
SELECT id, recording_id, attempts FROM enrichment_job
WHERE status='queued' AND next_attempt_at <= ? ORDER BY priority DESC, next_attempt_at, id LIMIT 1`,
		m.stamp(m.o.Now())).Scan(&j.id, &j.recordingID, &j.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enrichment_job SET status='running', updated_at=? WHERE id=?`,
		m.stamp(m.o.Now()), j.id); err != nil {
		return nil, err
	}
	return &j, tx.Commit()
}

func (m *Manager) handle(ctx context.Context, j job) {
	rc, ok, err := m.loadRecording(ctx, j.recordingID)
	if err != nil {
		m.fail(ctx, j, err)
		return
	}
	if !ok { // recording vanished (merged away); nothing to enrich
		m.finish(ctx, j.id, StatusDone, j.attempts, OutcomeNoMatch, "", time.Time{})
		return
	}
	retryLater := func(err error) bool {
		var rl *providers.RateLimitedError
		if !errors.As(err, &rl) {
			return false
		}
		wait := time.Duration(rl.RetryAfter) * time.Second
		if wait < time.Minute {
			wait = time.Minute
		}
		if wait > time.Hour {
			wait = time.Hour
		}
		m.finish(ctx, j.id, StatusQueued, j.attempts, "", err.Error(), m.o.Now().Add(wait))
		return true
	}

	// Phase 1: ask each provider about the ids the recording already holds
	// (a Spotify or Apple catalog id, a MusicBrainz id). This is exact: it
	// yields the ISRC, the credited artists with their ids, and the album.
	var held []heldTrack
	var firstErr error
	blocked := false // a held id could not be asked about yet (no credentials)
	have := map[string]bool{}
	stale := map[string][]string{} // held ids the album's tracklist contradicts, by provider
	for _, lk := range rc.links {
		have[lk.provider] = true
		p, ok := m.matcher.Providers.Get(lk.provider)
		if !ok {
			blocked = true
			continue
		}
		t, err := m.getTrack(ctx, p, lk.id)
		if err == nil && lk.replaceable() && len(rc.releaseIDs[lk.provider]) > 0 {
			off, offErr := m.offAlbum(ctx, p, t, rc.releaseIDs[lk.provider])
			if retryLater(offErr) {
				return
			}
			if off {
				// Matching chose a recording that is not on the album a person
				// linked (usually another edition's copy of the song). Leave it
				// unapplied and let the ladder read the album's tracklist.
				stale[lk.provider] = append(stale[lk.provider], lk.id)
				continue
			}
		}
		switch {
		case retryLater(err):
			return
		case err == nil:
			held = append(held, heldTrack{t: t, credits: trustedCredits(lk.method), linkID: lk.id, method: lk.method})
			rc.isrcs = append(rc.isrcs, t.ISRCs...)
			if rc.durationMs == 0 { // the lookup is the first place we learn it (stored below)
				rc.durationMs = t.DurationMs
			}
		case errors.Is(err, providers.ErrNotConfigured), errors.Is(err, providers.ErrUnsupported):
			blocked = true
		case errors.Is(err, providers.ErrNotFound):
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	for p := range stale {
		have[p] = false
		for _, lk := range rc.links {
			if lk.provider == p && !slices.Contains(stale[p], lk.id) {
				have[p] = true // another id it holds still stands
			}
		}
		if have[p] {
			delete(stale, p)
		}
	}

	// The album's release date, from our release or else from the lookups above:
	// the ladder uses it to tell editions apart, with no request of its own.
	if rc.releaseDate == "" {
		rc.releaseDate = m.albumDate(ctx, rc.album, held)
	}

	// High-precision mode: find the MusicBrainz recording through the album's
	// barcode and the track's position before asking by ISRC or name.
	var albumLink *match.Link
	if !have[providers.MusicBrainz] && len(held) > 0 && m.HighPrecision(ctx) {
		l, err := m.resolveByAlbum(ctx, rc, held)
		switch {
		case retryLater(err):
			return
		case err != nil:
			log.Printf("enrich: recording %d: album lookup: %v", j.recordingID, err)
		case l != nil:
			albumLink = l
			have[providers.MusicBrainz] = true // no need to ask MusicBrainz again below
		}
	}

	// Phase 2: the matching ladder against providers that hold no link yet,
	// with the ISRC when phase 1 found one (exact) and names otherwise (fuzzy).
	var res match.Result
	reg := providers.NewRegistry()
	for _, p := range m.matcher.Providers.All() {
		if !have[p.Name()] {
			reg.Add(p)
		}
	}
	if len(reg.All()) > 0 {
		l := match.Listen{Artist: strings.Join(rc.artists, ", "), Title: rc.title, Album: rc.album, DurationMs: rc.durationMs, ReleaseDate: rc.releaseDate, ReleaseIDs: rc.releaseIDs}
		if len(rc.isrcs) > 0 {
			l.ISRC = rc.isrcs[0]
		}
		res, err = match.New(reg, m.matcher.Config).Match(ctx, l)
		if retryLater(err) {
			return
		}
		if len(stale) > 0 {
			// Only a recording on the album may take the contradicted one's place.
			kept := res.Links[:0]
			for _, l := range res.Links {
				if len(stale[l.Provider]) == 0 || (l.Track != nil && onRelease(*l.Track, rc.releaseIDs[l.Provider])) {
					kept = append(kept, l)
				}
			}
			res.Links = kept
		}
		if !rc.fuzzyConsent { // the import asked for no automatic fuzzy matches
			for i := range res.Links {
				if res.Links[i].Method == match.MethodFuzzy && res.Links[i].Status == match.StatusAuto && !res.Links[i].Corroborated {
					res.Links[i].Status = match.StatusCandidate
				}
			}
		}
		if len(res.Links) == 0 && len(held) == 0 && res.Unavailable && len(rc.links) > 0 && !blocked {
			// The recording holds ids, every one was asked about and none is known
			// (a dead id), and the providers left have no credentials. Waiting for
			// keys would not change that answer, so the job is finished; adding a
			// provider later queues every recording again.
			m.finish(ctx, j.id, StatusDone, j.attempts, OutcomeNoMatch, "", time.Time{})
			return
		}
		if len(res.Links) == 0 && len(held) == 0 && res.Unavailable {
			// No provider could be asked (no credentials yet). Not an attempt
			// and not an answer: look again later, once keys may have been entered.
			m.finish(ctx, j.id, StatusQueued, j.attempts, "", "no provider available", m.o.Now().Add(time.Hour))
			return
		}
		if err != nil && len(res.Links) == 0 && len(held) == 0 {
			m.fail(ctx, j, err)
			return
		}
	} else if len(held) == 0 && len(m.matcher.Providers.All()) == 0 {
		// No provider registered at all: not an answer, look again later.
		m.finish(ctx, j.id, StatusQueued, j.attempts, "", "no provider available", m.o.Now().Add(time.Hour))
		return
	} else if len(held) == 0 && firstErr != nil {
		m.fail(ctx, j, firstErr)
		return
	}
	if albumLink != nil {
		res.Links = append([]match.Link{*albumLink}, res.Links...)
	}
	// The barcode of every album about to be attached, so Spotify's and Apple
	// Music's albums link to one release of ours only when they are the same
	// release, whatever their titles say.
	var toApply []*providers.Track
	for _, h := range held {
		toApply = append(toApply, h.t)
	}
	for _, l := range res.Links {
		if l.Status == match.StatusAuto && l.Track != nil {
			toApply = append(toApply, l.Track)
		}
	}
	albums, err := m.loadAlbums(ctx, toApply)
	if retryLater(err) {
		return
	}
	outcome, err := m.store(ctx, j.recordingID, held, res, albums, albumEvidence{ids: rc.releaseIDs, stale: stale})
	if err != nil {
		m.fail(ctx, j, err)
		return
	}
	m.namesDue.Store(true) // new links may need names (languages.go)
	m.finish(ctx, j.id, StatusDone, j.attempts, outcome, "", time.Time{})
}

// getTrack fetches a recording from a provider by an id the recording holds. An
// id the provider does not know as a recording may be a track id (histories
// mix them up: MusicBrainz's release-track ids look just like recording ids),
// so before giving up a provider that can resolve those is asked as well.
func (m *Manager) getTrack(ctx context.Context, p providers.Provider, id string) (*providers.Track, error) {
	t, err := m.getTrackByID(ctx, p, id)
	if !errors.Is(err, providers.ErrNotFound) {
		return t, err
	}
	tl, ok := p.(providers.TrackIDLookup)
	if !ok {
		return t, err
	}
	// Cached apart from recordings, so a "not found" from before this lookup
	// existed does not hide the answer, and a miss is remembered like any other.
	name := p.Name() + ".track"
	if m.snaps != nil {
		if e, err := m.snaps.Get(ctx, name, id, snapshot.KindRecording); err == nil && e != nil && e.Fresh(m.o.Now()) {
			if e.NotFound {
				return nil, providers.ErrNotFound
			}
			var t providers.Track
			if json.Unmarshal(e.Raw, &t) == nil && t.ProviderID != "" {
				markTrackDates(&t)
				return &t, nil
			}
		}
	}
	t, err = tl.LookupTrackID(ctx, id)
	if m.snaps != nil {
		switch {
		case errors.Is(err, providers.ErrNotFound):
			_ = m.snaps.PutNotFound(ctx, name, id, snapshot.KindRecording, 0)
		case err == nil:
			if b, e := json.Marshal(t); e == nil {
				_ = m.snaps.Put(ctx, name, id, snapshot.KindRecording, b, 0)
				if cur, e := m.snaps.Get(ctx, p.Name(), t.ProviderID, snapshot.KindRecording); e != nil || cur == nil || !cur.Fresh(m.o.Now()) || cur.NotFound {
					_ = m.snaps.Put(ctx, p.Name(), t.ProviderID, snapshot.KindRecording, b, 0)
				}
			}
		}
	}
	return t, err
}

// getTrackByID is getTrack for an id that is the provider's own recording id,
// through the snapshot cache (negative answers are cached too).
func (m *Manager) getTrackByID(ctx context.Context, p providers.Provider, id string) (*providers.Track, error) {
	if m.snaps != nil {
		if e, err := m.snaps.Get(ctx, p.Name(), id, snapshot.KindRecording); err == nil && e != nil && e.Fresh(m.o.Now()) {
			if e.NotFound {
				return nil, providers.ErrNotFound
			}
			var t providers.Track
			if json.Unmarshal(e.Raw, &t) == nil && t.ProviderID != "" {
				markTrackDates(&t)
				return &t, nil
			}
		}
	}
	t, err := p.GetTrack(ctx, id)
	if m.snaps != nil {
		switch {
		case errors.Is(err, providers.ErrNotFound):
			_ = m.snaps.PutNotFound(ctx, p.Name(), id, snapshot.KindRecording, 0)
		case err == nil:
			if b, e := json.Marshal(t); e == nil {
				_ = m.snaps.Put(ctx, p.Name(), id, snapshot.KindRecording, b, 0)
			}
		}
	}
	return t, err
}

// fail records an error and schedules a retry with backoff, or gives up.
func (m *Manager) fail(ctx context.Context, j job, err error) {
	attempts := j.attempts + 1
	if attempts >= m.o.MaxAttempts {
		m.finish(ctx, j.id, StatusFailed, attempts, "", err.Error(), time.Time{})
		return
	}
	backoff := time.Duration(1<<attempts) * time.Minute
	if backoff > time.Hour {
		backoff = time.Hour
	}
	m.finish(ctx, j.id, StatusQueued, attempts, "", err.Error(), m.o.Now().Add(backoff))
}

func (m *Manager) finish(ctx context.Context, id int64, status string, attempts int, outcome, lastErr string, next time.Time) {
	nextAt := m.stamp(m.o.Now())
	if !next.IsZero() {
		nextAt = m.stamp(next)
	}
	var out, le any
	if outcome != "" {
		out = outcome
	}
	if lastErr != "" {
		le = lastErr
	}
	if _, err := m.db.ExecContext(ctx, `
UPDATE enrichment_job SET status=?, attempts=?, outcome=?, last_error=?, next_attempt_at=?, updated_at=? WHERE id=?`,
		status, attempts, out, le, nextAt, m.stamp(m.o.Now()), id); err != nil {
		log.Printf("enrich: finishing job %d: %v", id, err)
	}
}

// recordingCtx is what we know about a recording: its names, whether fuzzy
// matches may be applied automatically, and the ids it already holds.
type recordingCtx struct {
	title        string
	durationMs   int
	artists      []string
	album        string
	releaseDate  string              // of our album, when known
	releaseIDs   map[string][]string // album ids by provider: ones the listen named or a person linked
	fuzzyConsent bool
	links        []heldLink // non-candidate provider ids other than ISRC
	isrcs        []string
}

type heldLink struct {
	provider, id, method string
	firm                 bool // set by a person (confirmed or pinned): never replaced
}

// replaceable reports whether a held link is one matching chose itself (by
// ISRC, lookup or name), not one the listen named or a person set, so a
// stronger answer may replace it.
func (l heldLink) replaceable() bool {
	return !l.firm && l.method != match.MethodSubmittedMBID && l.method != "submitted_id"
}

// heldTrack is a provider's answer for an id the recording holds. credits says
// the id came from the listen itself (a catalog id the user's own history
// named), so the answer's credit list may replace the recording's artists.
// Ids we found ourselves (ISRC, fuzzy search) never do: they may be wrong.
type heldTrack struct {
	t       *providers.Track
	credits bool
	// linkID and method are the id the recording holds and how it got it. They
	// differ from t.ProviderID when the id was a track id that resolved to a
	// recording; store then links the recording's own id as well.
	linkID, method string
}

func trustedCredits(method string) bool {
	return method == match.MethodSubmittedMBID || method == "submitted_id"
}

func (m *Manager) loadRecording(ctx context.Context, id int64) (recordingCtx, bool, error) {
	var rc recordingCtx
	var dur sql.NullInt64
	var consent int
	err := m.db.QueryRowContext(ctx, `SELECT title, duration_ms, fuzzy_consent FROM recording WHERE id=?`, id).Scan(&rc.title, &dur, &consent)
	if errors.Is(err, sql.ErrNoRows) {
		return rc, false, nil
	}
	if err != nil {
		return rc, false, err
	}
	rc.durationMs, rc.fuzzyConsent = int(dur.Int64), consent != 0
	rows, err := m.db.QueryContext(ctx, `
SELECT a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
WHERE ra.recording_id=? ORDER BY ra.position, a.id`, id)
	if err != nil {
		return rc, false, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return rc, false, err
		}
		rc.artists = append(rc.artists, n)
	}
	rows.Close()
	var album sql.NullString
	err = m.db.QueryRowContext(ctx, `
SELECT rel.title, COALESCE(rel.release_date, '') FROM recording_release rr JOIN release rel ON rel.id=rr.release_id
WHERE rr.recording_id=? ORDER BY rr.is_primary DESC, rel.id LIMIT 1`, id).Scan(&album, &rc.releaseDate)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rc, false, err
	}
	rc.album = album.String
	// Albums whose tracklist may place the recording: ones the listen named or
	// a person linked. Links matching found itself may be another edition.
	rows, err = m.db.QueryContext(ctx, `SELECT DISTINCT el.provider, el.provider_id FROM recording_release rr
JOIN external_link el ON el.entity_type='release' AND el.entity_id=rr.release_id
WHERE rr.recording_id=? AND el.provider IN ('musicbrainz', 'spotify', 'apple') AND el.status <> 'candidate'
  AND (el.match_method IN ('submitted_mbid', 'submitted_id') OR el.status='confirmed' OR el.pinned=1)
ORDER BY el.provider, el.provider_id`, id)
	if err != nil {
		return rc, false, err
	}
	for rows.Next() {
		var prov, rel string
		if err := rows.Scan(&prov, &rel); err != nil {
			rows.Close()
			return rc, false, err
		}
		if rc.releaseIDs == nil {
			rc.releaseIDs = map[string][]string{}
		}
		rc.releaseIDs[prov] = append(rc.releaseIDs[prov], rel)
	}
	rows.Close()
	rows, err = m.db.QueryContext(ctx, `SELECT provider, provider_id, match_method, status='confirmed' OR pinned=1 FROM external_link
WHERE entity_type='recording' AND entity_id=? AND status <> 'candidate' ORDER BY id`, id)
	if err != nil {
		return rc, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var l heldLink
		if err := rows.Scan(&l.provider, &l.id, &l.method, &l.firm); err != nil {
			return rc, false, err
		}
		if l.provider == "isrc" {
			rc.isrcs = append(rc.isrcs, l.id)
		} else {
			rc.links = append(rc.links, l)
		}
	}
	return rc, true, rows.Err()
}

// store writes the ladder's links. Rejected candidates are skipped, and
// candidates are dropped for a provider that already produced an auto link.
// albumEvidence is what the albums a person linked (or the listen named) say
// about a recording: their ids by provider, and the held links their
// tracklists contradict, which an auto link from the same provider replaces.
type albumEvidence struct {
	ids, stale map[string][]string
}

func (m *Manager) store(ctx context.Context, recID int64, held []heldTrack, res match.Result, albums map[string]providers.Album, ev albumEvidence) (string, error) {
	autoFor := map[string]bool{}
	for _, l := range res.Links {
		if l.Status == match.StatusAuto {
			autoFor[l.Provider] = true
		}
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	outcome := OutcomeNoMatch
	// Ids the listen named come first: their albums are the ones that were
	// played, so they set a release's barcode before an Apple song we merely
	// found by ISRC (and may sit on another edition) can.
	ordered := make([]heldTrack, 0, len(held))
	for _, trusted := range []bool{true, false} {
		for _, h := range held {
			if h.credits == trusted {
				ordered = append(ordered, h)
			}
		}
	}
	var applied []albumSource // every track applied, for the release gap fill below
	creditsSet := false
	for _, h := range ordered { // exact lookups by ids the recording holds
		// The first catalog id the listen itself named sets the credits; every
		// other lookup only attaches its ids to those artists.
		setCredits := h.credits && !creditsSet
		if h.linkID != "" && h.t.ProviderID != "" && h.linkID != h.t.ProviderID {
			if err := linkResolvedID(ctx, tx, recID, h); err != nil {
				return "", err
			}
		}
		if err := applyTrack(ctx, tx, recID, h.t, h.credits, setCredits, albums); err != nil {
			return "", err
		}
		creditsSet = creditsSet || setCredits
		applied = append(applied, albumSource{h.t, h.credits})
		outcome = OutcomeLinked
	}
	for _, l := range res.Links {
		if l.Status == match.StatusCandidate && autoFor[l.Provider] {
			continue
		}
		var rejected int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM link_rejection
WHERE entity_type='recording' AND entity_id=? AND provider=? AND provider_id=?`,
			recID, l.Provider, l.ProviderID).Scan(&rejected); err != nil {
			return "", err
		}
		if rejected > 0 {
			continue
		}
		if l.Status == match.StatusAuto {
			// An id another recording already holds is not moved: the two
			// recordings are probably duplicates, flagged for a manual merge.
			owner, err := identity.FindByLink(ctx, tx, identity.KindRecording, l.Provider, l.ProviderID)
			if err != nil {
				return "", err
			}
			if owner != 0 && owner != recID {
				same, err := sameAlbumTrack(ctx, tx, owner, l, ev.ids[l.Provider])
				if err != nil {
					return "", err
				}
				if same {
					// Both are the one track of an album they share: a duplicate,
					// merged automatically (see autoMergeRecordings). The links
					// the tracklist disproved go, so they do not block the merge.
					if err = dropStale(ctx, tx, recID, l.Provider, ev.stale[l.Provider], l.ProviderID); err == nil {
						err = flagSameAlbumTrack(ctx, tx, recID, owner)
					}
				} else {
					err = identity.Candidate(ctx, tx, identity.KindRecording, recID, owner, "same "+l.Provider+" id")
				}
				if err != nil {
					return "", err
				}
				if outcome == OutcomeNoMatch {
					outcome = OutcomeCandidates
				}
				continue
			}
		}
		if l.Status == match.StatusAuto {
			if err := dropStale(ctx, tx, recID, l.Provider, ev.stale[l.Provider], l.ProviderID); err != nil {
				return "", err
			}
		}
		// A candidate an earlier scan left is promoted when this scan finds the
		// evidence for it; auto, confirmed and rejected links are never touched.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status)
VALUES ('recording', ?, ?, ?, ?, ?, ?)
ON CONFLICT(entity_type, entity_id, provider, provider_id) DO UPDATE SET
  match_method=excluded.match_method, confidence=excluded.confidence, status=excluded.status
  WHERE external_link.status='candidate' AND excluded.status='auto'`,
			recID, l.Provider, l.ProviderID, l.Method, l.Confidence, l.Status); err != nil {
			return "", err
		}
		if l.Status == match.StatusAuto {
			// The other candidates this provider offered earlier were rivals of the
			// one now chosen; they are no longer open questions.
			if _, err := tx.ExecContext(ctx, `DELETE FROM external_link WHERE entity_type='recording' AND entity_id=?
AND provider=? AND status='candidate' AND provider_id<>?`, recID, l.Provider, l.ProviderID); err != nil {
				return "", err
			}
			outcome = OutcomeLinked
			if l.Track != nil {
				if err := applyTrack(ctx, tx, recID, l.Track, false, false, albums); err != nil {
					return "", err
				}
				applied = append(applied, albumSource{l.Track, false})
			}
		} else if outcome == OutcomeNoMatch {
			outcome = OutcomeCandidates
		}
	}
	if err := fillReleaseGaps(ctx, tx, recID, applied, albums); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	// Cache provider data behind each link so the review UI and later
	// enrichment need no refetch. Best effort: the links are already stored.
	if m.snaps != nil {
		for _, l := range res.Links {
			if l.Track == nil {
				continue
			}
			b, err := json.Marshal(l.Track)
			if err == nil {
				err = m.snaps.Put(ctx, l.Provider, l.ProviderID, snapshot.KindRecording, b, 0)
			}
			if err != nil {
				log.Printf("enrich: caching %s/%s: %v", l.Provider, l.ProviderID, err)
			}
		}
	}
	return outcome, nil
}

// Counts reports queue depth by status, for the admin status endpoint.
func (m *Manager) Counts(ctx context.Context) (map[string]int64, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM enrichment_job GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{StatusQueued: 0, StatusRunning: 0, StatusDone: 0, StatusFailed: 0}
	for rows.Next() {
		var s string
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var cand int64
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT entity_id) FROM external_link
WHERE entity_type='recording' AND status='candidate'`).Scan(&cand); err != nil {
		return nil, err
	}
	out["review_pending"] = cand
	return out, nil
}

// linkResolvedID records the recording id a held track id resolved to, as its
// own link, beside the track id the listen named (kept, so the same listen
// resolves to this recording again). If another recording already holds that
// id the two are probably duplicates and are flagged, never moved or merged.
func linkResolvedID(ctx context.Context, tx *sql.Tx, recID int64, h heldTrack) error {
	owner, err := identity.FindByLink(ctx, tx, identity.KindRecording, h.t.Provider, h.t.ProviderID)
	if err != nil {
		return err
	}
	if owner != 0 && owner != recID {
		return identity.Candidate(ctx, tx, identity.KindRecording, recID, owner, "same "+h.t.Provider+" id")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status)
VALUES ('recording', ?, ?, ?, ?, 1.0, 'auto')
ON CONFLICT(entity_type, entity_id, provider, provider_id) DO NOTHING`, recID, h.t.Provider, h.t.ProviderID, h.method)
	return err
}

// onRelease reports whether a catalogue track lists one of the releases.
func onRelease(t providers.Track, releases []string) bool {
	for _, r := range t.Releases {
		for _, id := range releases {
			if strings.EqualFold(r.ProviderID, id) {
				return true
			}
		}
	}
	return false
}

// offAlbum reports whether the album's tracklist proves t is not on it: t lists
// none of the releases, and at least one of their tracklists was read without
// holding it. A recording's own release list can be cut short, so only the
// tracklist counts as proof; with no tracklist read, nothing is proven.
func (m *Manager) offAlbum(ctx context.Context, p providers.Provider, t *providers.Track, releases []string) (bool, error) {
	if onRelease(*t, releases) {
		return false, nil
	}
	rl, ok := p.(providers.ReleaseTracksLookup)
	if !ok {
		return false, nil
	}
	read := false
	for _, id := range releases {
		tracks, err := rl.LookupReleaseTracks(ctx, id)
		if err != nil {
			if isRateLimited(err) {
				return false, err
			}
			continue // a dead or unreadable release proves nothing
		}
		read = true
		for _, x := range tracks {
			if strings.EqualFold(x.ProviderID, t.ProviderID) {
				return false, nil
			}
		}
	}
	return read, nil
}

// ReasonSameAlbumTrack flags two recordings the tracklist of an album both
// are on names as one track.
const ReasonSameAlbumTrack = "same track on the album"

// sameAlbumTrack reports whether l, a track another recording (owner) holds,
// was read off the tracklist of an album the recording is on (albumIDs, ones
// the listen named or a person linked), and owner is on that album of ours too.
func sameAlbumTrack(ctx context.Context, tx *sql.Tx, owner int64, l match.Link, albumIDs []string) (bool, error) {
	if l.Track == nil {
		return false, nil
	}
	for _, r := range l.Track.Releases {
		if r.ProviderID == "" || !slices.ContainsFunc(albumIDs, func(id string) bool { return strings.EqualFold(id, r.ProviderID) }) {
			continue
		}
		var on bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM recording_release rr
JOIN external_link el ON el.entity_type='release' AND el.entity_id=rr.release_id
WHERE rr.recording_id=? AND el.provider=? AND el.provider_id=? COLLATE NOCASE AND el.status <> 'candidate')`,
			owner, l.Provider, r.ProviderID).Scan(&on)
		if err != nil || on {
			return on, err
		}
	}
	return false, nil
}

// flagSameAlbumTrack records the pair as one track of an album, replacing a
// weaker reason an earlier scan gave it.
func flagSameAlbumTrack(ctx context.Context, tx *sql.Tx, a, b int64) error {
	if a > b {
		a, b = b, a
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO merge_candidate (kind, entity_id, other_id, reason) VALUES ('recording', ?, ?, ?)
ON CONFLICT(kind, entity_id, other_id) DO UPDATE SET reason=excluded.reason`, a, b, ReasonSameAlbumTrack)
	return err
}

// mergeUndone reports whether a person undid the merge of from into into, so
// matching leaves the pair for a person instead of merging it again.
func (m *Manager) mergeUndone(ctx context.Context, kind string, from, into int64) bool {
	var n int
	err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edit_log WHERE scope='merge' AND entity_type=? AND entity_id=?
  AND summary LIKE ? AND undone_at IS NOT NULL`, kind, into, fmt.Sprintf("Merge %s %d into %d%%", kind, from, into)).Scan(&n)
	return err == nil && n > 0
}

// dropStale removes a recording's links that an album's tracklist disproved,
// except keep (the id taking their place).
func dropStale(ctx context.Context, tx *sql.Tx, recID int64, provider string, stale []string, keep string) error {
	for _, old := range stale {
		if old == keep {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM external_link WHERE entity_type='recording' AND entity_id=?
AND provider=? AND provider_id=? AND status='auto' AND pinned=0`, recID, provider, old); err != nil {
			return err
		}
	}
	return nil
}
