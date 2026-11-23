package enrich

import "context"

// fillTrackPositions gives tracks with no place on their album the disc and
// track number a catalogue already told us, from the answers we cached. The
// catalogue's album must be the one our release is linked to (same provider,
// same album id), and the track's link must be trusted. Spotify's answer comes
// first, then Apple's, then iTunes', then MusicBrainz's. A position we already
// have is never changed. It returns how many tracks it filled.
//
// CROSS JOIN fixes the join order: start from the tracks missing a position
// and reach every other table by a unique key. Left to choose, SQLite paired
// every track link with every album link of the same catalogue, which took
// minutes on a large library while holding the only write connection.
func (m *Manager) fillTrackPositions(ctx context.Context) (int64, error) {
	res, err := m.db.ExecContext(ctx, fillPositionsSQL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const fillPositionsSQL = `
WITH cand AS (
  SELECT rr.recording_id AS rec, rr.release_id AS rel,
         MAX(1, COALESCE(json_extract(r.value, '$.Disc'), 1)) AS disc,
         json_extract(r.value, '$.Position') AS pos,
         ROW_NUMBER() OVER (PARTITION BY rr.recording_id, rr.release_id ORDER BY
           CASE l.provider WHEN 'spotify' THEN 0 WHEN 'apple' THEN 1 WHEN 'itunes' THEN 2 ELSE 3 END) AS rn
  FROM recording_release rr
  CROSS JOIN external_link l ON l.entity_type = 'recording' AND l.entity_id = rr.recording_id
  CROSS JOIN provider_snapshot s ON s.provider = l.provider AND s.provider_id = l.provider_id AND s.kind = 'recording' AND s.status = 'ok'
  CROSS JOIN json_each(s.raw_json, '$.Releases') r
  CROSS JOIN external_link rl ON rl.entity_type = 'release' AND rl.entity_id = rr.release_id AND rl.provider = l.provider
       AND rl.provider_id = json_extract(r.value, '$.ProviderID') AND rl.status IN ('confirmed', 'auto')
  WHERE rr.position IS NULL AND COALESCE(json_extract(r.value, '$.Position'), 0) > 0 AND ` + trustedTrackLink + `
)
UPDATE recording_release SET disc = c.disc, position = c.pos
FROM cand c
WHERE c.rn = 1 AND recording_release.recording_id = c.rec AND recording_release.release_id = c.rel
  AND recording_release.position IS NULL`
