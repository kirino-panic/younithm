package enrich

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strconv"
	"time"

	"younithm/internal/identity"
	"younithm/internal/providers"
)

// settingCreditedBackfill holds the credited-name backfill's progress: the
// last release id done, or "done" once every release has been through it.
const settingCreditedBackfill = "backfill.credited_names"

// creditedBatch is how many releases one idle tick backfills. At MusicBrainz's
// one request a second it keeps a tick short, so new listens are never kept
// waiting behind it.
const creditedBatch = 10

// creditedRetry is how long the backfill waits after MusicBrainz fails.
const creditedRetry = 10 * time.Minute

// creditedNamesPass fills in credited names ("The Square" for T-SQUARE) on
// albums and tracks enriched before they were stored. For each release linked
// to MusicBrainz, in id order, it fetches the release once (album credit and
// every track's credit in one request) and fills the gaps the way a fresh
// lookup would: by MusicBrainz artist id only, never overwriting, never on
// anything edited by hand. Progress is kept in admin_setting, so a restart
// carries on where it stopped. It reports whether work remains.
func (m *Manager) creditedNamesPass(ctx context.Context) (bool, error) {
	if m.creditedDone.Load() || m.o.Now().Before(m.creditedPause) {
		return false, nil
	}
	p, ok := m.matcher.Providers.Get(providers.MusicBrainz)
	if !ok {
		return false, nil
	}
	rl, ok := p.(providers.ReleaseTracksLookup)
	if !ok {
		return false, nil
	}
	var cur string
	err := m.db.QueryRowContext(ctx, `SELECT value FROM admin_setting WHERE key=?`, settingCreditedBackfill).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if cur == "done" {
		m.creditedDone.Store(true)
		return false, nil
	}
	last, _ := strconv.ParseInt(cur, 10, 64)

	type rel struct {
		id   int64
		mbid string
	}
	rows, err := m.db.QueryContext(ctx, `SELECT l.entity_id, MIN(l.provider_id) FROM external_link l
WHERE l.entity_type='release' AND l.provider=? AND l.status IN ('confirmed','auto') AND l.entity_id > ?
GROUP BY l.entity_id ORDER BY l.entity_id LIMIT ?`, identity.MusicBrainz, last, creditedBatch)
	if err != nil {
		return false, err
	}
	var rels []rel
	for rows.Next() {
		var r rel
		if err := rows.Scan(&r.id, &r.mbid); err != nil {
			rows.Close()
			return false, err
		}
		rels = append(rels, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(rels) == 0 {
		m.creditedDone.Store(true)
		return false, m.setCreditedCursor(ctx, "done")
	}

	for _, r := range rels {
		tracks, err := rl.LookupReleaseTracks(ctx, r.mbid)
		if err != nil && !errors.Is(err, providers.ErrNotFound) {
			m.creditedPause = m.o.Now().Add(creditedRetry)
			return true, err // the cursor stays: this release is tried again
		}
		if err := m.fillCreditedFrom(ctx, r.id, r.mbid, tracks); err != nil {
			return true, err
		}
		if err := m.setCreditedCursor(ctx, strconv.FormatInt(r.id, 10)); err != nil {
			return true, err
		}
	}
	return true, nil
}

// fillCreditedFrom applies one MusicBrainz release's tracklist: the album
// credit to release relID, each track's credit to the recording holding that
// MusicBrainz recording id.
func (m *Manager) fillCreditedFrom(ctx context.Context, relID int64, mbid string, tracks []providers.Track) error {
	if len(tracks) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ref := range tracks[0].Releases {
		if ref.ProviderID == mbid {
			if err := fillCreditedNames(ctx, tx, identity.KindRelease, relID, providers.MusicBrainz, ref.Artists); err != nil {
				return err
			}
			break
		}
	}
	for _, t := range tracks {
		if t.ProviderID == "" {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT entity_id FROM external_link
WHERE entity_type='recording' AND provider=? AND provider_id=? AND status IN ('confirmed','auto')`, identity.MusicBrainz, t.ProviderID)
		if err != nil {
			return err
		}
		var recs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				recs = append(recs, id)
			}
		}
		rows.Close()
		for _, rec := range recs {
			if err := fillCreditedNames(ctx, tx, identity.KindRecording, rec, providers.MusicBrainz, t.Artists); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (m *Manager) setCreditedCursor(ctx context.Context, v string) error {
	_, err := m.db.ExecContext(ctx, `INSERT INTO admin_setting (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`, settingCreditedBackfill, v)
	if err != nil {
		log.Printf("enrich: credited-name backfill progress: %v", err)
	}
	return err
}
