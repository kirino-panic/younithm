package debugui

import (
	"net/http"

	"younithm/internal/identity"
)

// stats answers "what is in the database right now": object counts, play
// counts, how the provider links are spread, queue state and the identity
// integrity checks. Every number is a plain COUNT so it can be checked by hand.
func (d Deps) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var firstErr error
	n := func(q string) int64 {
		v, err := count(ctx, d.Read, q)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	}
	rows := func(q string) []row {
		v, err := query(ctx, d.Read, q)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	}

	objects := map[string]int64{
		"scrobbles (plays)":   n(`SELECT COUNT(*) FROM scrobble`),
		"recordings (tracks)": n(`SELECT COUNT(*) FROM recording`),
		"releases (albums)":   n(`SELECT COUNT(*) FROM release`),
		"artists":             n(`SELECT COUNT(*) FROM artist`),
		"external links":      n(`SELECT COUNT(*) FROM external_link`),
		"aliases":             n(`SELECT COUNT(*) FROM alias`),
		"provider snapshots":  n(`SELECT COUNT(*) FROM provider_snapshot`),
		"merge candidates":    n(`SELECT COUNT(*) FROM merge_candidate`),
		"link rejections":     n(`SELECT COUNT(*) FROM link_rejection`),
		"edit log entries":    n(`SELECT COUNT(*) FROM edit_log`),
		"import jobs":         n(`SELECT COUNT(*) FROM import_job`),
	}

	plays := map[string]any{
		"total":                      objects["scrobbles (plays)"],
		"distinct recordings played": n(`SELECT COUNT(DISTINCT recording_id) FROM scrobble`),
		"distinct releases played":   n(`SELECT COUNT(DISTINCT release_id) FROM scrobble`),
		"with a recording":           n(`SELECT COUNT(*) FROM scrobble WHERE recording_id IS NOT NULL`),
		"with a release":             n(`SELECT COUNT(*) FROM scrobble WHERE release_id IS NOT NULL`),
		"with a raw payload":         n(`SELECT COUNT(*) FROM scrobble WHERE raw_payload IS NOT NULL`),
		"earliest":                   rows(`SELECT MIN(played_at) AS at FROM scrobble`),
		"latest":                     rows(`SELECT MAX(played_at) AS at FROM scrobble`),
		"by source":                  rows(`SELECT COALESCE(source,'(none)') AS source, COUNT(*) AS plays FROM scrobble GROUP BY 1 ORDER BY 2 DESC`),
		"by import job":              rows(`SELECT import_job_id, COUNT(*) AS plays FROM scrobble WHERE import_job_id IS NOT NULL GROUP BY 1 ORDER BY 1 DESC LIMIT 10`),
	}

	artists := map[string]int64{
		"verified (a provider id corroborates)": n(`SELECT COUNT(*) FROM artist WHERE verified=1`),
		"unverified":                            n(`SELECT COUNT(*) FROM artist WHERE verified=0`),
		"with any external link":                n(`SELECT COUNT(DISTINCT entity_id) FROM external_link WHERE entity_type='artist'`),
		"with no recordings":                    n(`SELECT COUNT(*) FROM artist a WHERE NOT EXISTS (SELECT 1 FROM recording_artist ra WHERE ra.artist_id=a.id)`),
	}
	recordings := map[string]int64{
		"with any external link":     n(`SELECT COUNT(DISTINCT entity_id) FROM external_link WHERE entity_type='recording'`),
		"with no link at all":        n(`SELECT COUNT(*) FROM recording r WHERE NOT EXISTS (SELECT 1 FROM external_link l WHERE l.entity_type='recording' AND l.entity_id=r.id)`),
		"with an ISRC":               n(`SELECT COUNT(DISTINCT entity_id) FROM external_link WHERE entity_type='recording' AND provider='isrc'`),
		"with no release":            n(`SELECT COUNT(*) FROM recording r WHERE NOT EXISTS (SELECT 1 FROM recording_release rr WHERE rr.recording_id=r.id)`),
		"with 2+ credited artists":   n(`SELECT COUNT(*) FROM (SELECT recording_id FROM recording_artist GROUP BY recording_id HAVING COUNT(*)>1)`),
		"fuzzy matching not allowed": n(`SELECT COUNT(*) FROM recording WHERE fuzzy_consent=0`),
		"never played":               n(`SELECT COUNT(*) FROM recording r WHERE NOT EXISTS (SELECT 1 FROM scrobble s WHERE s.recording_id=r.id)`),
	}
	releases := map[string]int64{
		"with any external link":  n(`SELECT COUNT(DISTINCT entity_id) FROM external_link WHERE entity_type='release'`),
		"with no album artist":    n(`SELECT COUNT(*) FROM release r WHERE NOT EXISTS (SELECT 1 FROM release_artist ra WHERE ra.release_id=r.id)`),
		"with a release date":     n(`SELECT COUNT(*) FROM release WHERE release_date IS NOT NULL AND release_date <> ''`),
		"with a release group id": n(`SELECT COUNT(*) FROM release WHERE release_group_mbid IS NOT NULL`),
		"with artwork":            n(`SELECT COUNT(*) FROM release WHERE artwork_path IS NOT NULL AND artwork_path <> ''`),
	}

	links := map[string]any{
		"by entity, provider, status": rows(`SELECT entity_type, provider, status, COUNT(*) AS links FROM external_link GROUP BY 1,2,3 ORDER BY 1,2,3`),
		"by match method":             rows(`SELECT match_method, COUNT(*) AS links FROM external_link GROUP BY 1 ORDER BY 2 DESC`),
	}
	enrichment := map[string]any{
		"jobs by status and outcome": rows(`SELECT status, COALESCE(outcome,'') AS outcome, COUNT(*) AS jobs FROM enrichment_job GROUP BY 1,2 ORDER BY 1,2`),
		"recordings with no job":     n(`SELECT COUNT(*) FROM recording r WHERE NOT EXISTS (SELECT 1 FROM enrichment_job j WHERE j.recording_id=r.id)`),
		"recent errors":              rows(`SELECT recording_id, attempts, last_error, updated_at FROM enrichment_job WHERE last_error IS NOT NULL AND last_error <> '' ORDER BY updated_at DESC LIMIT 10`),
	}

	top := map[string]any{
		"artists": rows(`SELECT ra.artist_id AS artist_id, a.name AS name, COUNT(*) AS plays
			FROM scrobble s JOIN recording_artist ra ON ra.recording_id=s.recording_id JOIN artist a ON a.id=ra.artist_id
			GROUP BY ra.artist_id ORDER BY plays DESC, a.id LIMIT 10`),
		"tracks": rows(`SELECT s.recording_id AS recording_id, rec.title AS title, COUNT(*) AS plays
			FROM scrobble s JOIN recording rec ON rec.id=s.recording_id
			GROUP BY s.recording_id ORDER BY plays DESC, rec.id LIMIT 10`),
		"albums": rows(`SELECT s.release_id AS release_id, rel.title AS title, COUNT(*) AS plays
			FROM scrobble s JOIN release rel ON rel.id=s.release_id
			GROUP BY s.release_id ORDER BY plays DESC, rel.id LIMIT 10`),
	}

	integrity, err := identity.CheckIntegrity(ctx, d.Read)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		serverErr(w, "stats", firstErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"objects": objects, "plays": plays, "artists": artists, "recordings": recordings,
		"releases": releases, "external_links": links, "enrichment": enrichment,
		"top": top, "integrity": integrity,
	})
}
