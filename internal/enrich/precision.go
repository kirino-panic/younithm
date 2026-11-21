package enrich

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"

	"younithm/internal/identity"
	"younithm/internal/providers"
	"younithm/internal/providers/match"
	"younithm/internal/providers/snapshot"
)

// High-precision mode (admin setting "matching.high_precision", on unless
// the admin turned it off). The normal ladder costs one MusicBrainz request per recording and
// already uses what those answers contain (release dates, durations). The mode
// spends more requests on albums to be sure instead: a catalogue album's
// barcode finds the MusicBrainz release, and the recording at the same disc and
// position on that release is the match, without comparing any names. It costs
// about one extra MusicBrainz request per album (a barcode search, then the
// release), and saves the recording's own ISRC lookup when it succeeds.
const settingHighPrecision = "matching.high_precision"

// HighPrecision reports whether high-precision matching is on. Never set
// means on.
func (m *Manager) HighPrecision(ctx context.Context) bool {
	var v string
	err := m.db.QueryRowContext(ctx, `SELECT value FROM admin_setting WHERE key=?`, settingHighPrecision).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return err == nil && v == "1"
}

// SetHighPrecision turns high-precision matching on or off. It applies to
// recordings matched from now on; queue them again to apply it to the library.
func (m *Manager) SetHighPrecision(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	_, err := m.db.ExecContext(ctx, `INSERT INTO admin_setting (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		settingHighPrecision, v)
	return err
}

// markTrackDates flags the release dates of an Apple Music or iTunes track as
// the song's own. The providers set it themselves, but answers cached before
// the flag existed (they live 30 days) do not carry it.
func markTrackDates(t *providers.Track) {
	if t.Provider != providers.AppleMusic && t.Provider != providers.ITunes {
		return
	}
	for i := range t.Releases {
		if t.Releases[i].Date != "" {
			t.Releases[i].DateIsTrack = true
		}
	}
}

// pickRelease returns the release of t that is the album the recording is on:
// the one with an equal title, else the only one.
func pickRelease(rs []providers.ReleaseRef, album string) *providers.ReleaseRef {
	for i := range rs {
		if album != "" && identity.Norm(rs[i].Title) == identity.Norm(album) {
			return &rs[i]
		}
	}
	if len(rs) == 1 {
		return &rs[0]
	}
	return nil
}

// albumDate is the release date of the album the recording is on, as the
// lookups of the ids it holds report it (ours is not stored until they are
// applied). A date that is the album's own (Spotify, MusicBrainz) is used as it
// is. Apple Music and iTunes report the song's date instead, which differs from
// the album's on compilations and reissues; for those the catalogue is asked
// for the album (one cached call to a service we do not throttle) and the track's
// reference is corrected in place, so the album's date is what gets stored. A
// date that cannot be established is left empty: it then says nothing.
func (m *Manager) albumDate(ctx context.Context, album string, held []heldTrack) string {
	for _, h := range held {
		if r := pickRelease(h.t.Releases, album); r != nil && r.Date != "" && !r.DateIsTrack {
			return r.Date
		}
	}
	for _, h := range held {
		r := pickRelease(h.t.Releases, album)
		if r == nil || !r.DateIsTrack || r.ProviderID == "" {
			continue
		}
		p, ok := m.matcher.Providers.Get(h.t.Provider)
		if !ok {
			continue
		}
		al, ok := p.(providers.AlbumLookup)
		if !ok {
			continue
		}
		if a, err := m.getAlbum(ctx, al, h.t.Provider, r.ProviderID); err == nil && a.Date != "" {
			r.Date, r.DateIsTrack = a.Date, false
			return a.Date
		}
	}
	return ""
}

// resolveByAlbum finds the MusicBrainz recording by album: the album's barcode
// (from a catalogue that holds the recording by an id), the MusicBrainz release
// with that barcode, and the track at the same disc and position. It links only
// when nothing contradicts it: one release carries the barcode, the durations
// agree and, when both sides list ISRCs, they share one. A nil link means "could
// not tell"; the normal ladder then runs as usual.
func (m *Manager) resolveByAlbum(ctx context.Context, rc recordingCtx, held []heldTrack) (*match.Link, error) {
	mbp, ok := m.matcher.Providers.Get(providers.MusicBrainz)
	if !ok {
		return nil, nil
	}
	bl, ok := mbp.(providers.BarcodeLookup)
	if !ok {
		return nil, nil
	}
	// A catalogue id the listen itself named identifies the album that was
	// played. One we found ourselves (an Apple song found by ISRC, say) may sit on
	// another edition of the album, so those are tried last and only when their
	// album is the one we know.
	ordered := make([]heldTrack, 0, len(held))
	for _, trusted := range []bool{true, false} {
		for _, h := range held {
			if h.credits == trusted {
				ordered = append(ordered, h)
			}
		}
	}
	for _, h := range ordered {
		rel := pickRelease(h.t.Releases, rc.album)
		if rel == nil || rel.ProviderID == "" || rel.Position < 1 {
			continue
		}
		p, ok := m.matcher.Providers.Get(h.t.Provider)
		if !ok {
			continue
		}
		al, ok := p.(providers.AlbumLookup)
		if !ok {
			continue
		}
		album, err := m.getAlbum(ctx, al, h.t.Provider, rel.ProviderID)
		if err != nil {
			if isRateLimited(err) {
				return nil, err
			}
			continue
		}
		if album.UPC == "" {
			continue
		}
		if agree, ok := match.DatesAgree(rc.releaseDate, album.Date); ok && !agree {
			continue // another edition of the album than the one the recording is on
		}
		releases, err := m.getBarcode(ctx, bl, album.UPC)
		if err != nil {
			return nil, err
		}
		var pick *providers.Release
		switch {
		case len(releases) == 1:
			pick = &releases[0]
		case len(releases) > 1:
			for i := range releases { // editions sharing a barcode: the one with this many tracks
				if album.TotalTracks > 0 && len(releases[i].Tracks) == album.TotalTracks {
					if pick != nil {
						pick = nil // two fit: ambiguous
						break
					}
					pick = &releases[i]
				}
			}
		}
		if pick == nil {
			continue
		}
		disc := rel.Disc
		if disc < 1 {
			disc = 1
		}
		var rt *providers.ReleaseTrack
		for i := range pick.Tracks {
			if pick.Tracks[i].Disc == disc && pick.Tracks[i].Position == rel.Position {
				rt = &pick.Tracks[i]
			}
		}
		if rt == nil || rt.RecordingID == "" {
			continue
		}
		tol := m.matcher.Config.DurationToleranceMs
		shifted := false
		for _, d := range []int{h.t.DurationMs, rc.durationMs} { // the catalogue's and our own
			if d > 0 && rt.DurationMs > 0 && absInt(d-rt.DurationMs) > tol {
				shifted = true // not the same recording: a different edit or a shifted tracklist
			}
		}
		if shifted {
			continue
		}
		conf := 0.95
		if len(rt.ISRCs) > 0 && len(h.t.ISRCs) > 0 {
			if !sharesISRC(rt.ISRCs, h.t.ISRCs) {
				continue // the ISRCs disagree
			}
			conf = 1
		}
		mbTrack, err := m.getTrack(ctx, mbp, rt.RecordingID)
		if err != nil {
			if isRateLimited(err) {
				return nil, err
			}
			continue
		}
		return &match.Link{Provider: providers.MusicBrainz, ProviderID: rt.RecordingID, Method: match.MethodLookup,
			Confidence: conf, Status: match.StatusAuto, Track: mbTrack}, nil
	}
	return nil, nil
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func sharesISRC(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if identity.Norm(x) == identity.Norm(y) {
				return true
			}
		}
	}
	return false
}

func isRateLimited(err error) bool {
	var rl *providers.RateLimitedError
	return errors.As(err, &rl)
}

// getAlbum asks the catalogue for an album, through the snapshot cache
// (answers and misses are both cached).
func (m *Manager) getAlbum(ctx context.Context, al providers.AlbumLookup, provider, id string) (*providers.Album, error) {
	if m.snaps != nil {
		if e, err := m.snaps.Get(ctx, provider, id, snapshot.KindRelease); err == nil && e != nil && e.Fresh(m.o.Now()) {
			if e.NotFound {
				return nil, providers.ErrNotFound
			}
			var a providers.Album
			if json.Unmarshal(e.Raw, &a) == nil && a.ProviderID != "" {
				return &a, nil
			}
		}
	}
	a, err := al.GetAlbum(ctx, id)
	if m.snaps != nil {
		switch {
		case errors.Is(err, providers.ErrNotFound):
			_ = m.snaps.PutNotFound(ctx, provider, id, snapshot.KindRelease, 0)
		case err == nil:
			if b, e := json.Marshal(a); e == nil {
				_ = m.snaps.Put(ctx, provider, id, snapshot.KindRelease, b, 0)
			}
		}
	}
	return a, err
}

// getBarcode asks MusicBrainz for the releases with a barcode, through the
// snapshot cache. "No release has this barcode" is cached too, so a library
// does not ask again for every recording of an album MusicBrainz lacks.
func (m *Manager) getBarcode(ctx context.Context, bl providers.BarcodeLookup, upc string) ([]providers.Release, error) {
	key := "barcode:" + upc
	if m.snaps != nil {
		if e, err := m.snaps.Get(ctx, providers.MusicBrainz, key, snapshot.KindRelease); err == nil && e != nil && e.Fresh(m.o.Now()) {
			if e.NotFound {
				return nil, nil
			}
			var rs []providers.Release
			if json.Unmarshal(e.Raw, &rs) == nil && len(rs) > 0 {
				return rs, nil
			}
		}
	}
	rs, err := bl.LookupBarcode(ctx, upc)
	if err != nil {
		if !errors.Is(err, providers.ErrNotFound) {
			return nil, err
		}
		rs = nil
	}
	if m.snaps != nil {
		if len(rs) == 0 {
			_ = m.snaps.PutNotFound(ctx, providers.MusicBrainz, key, snapshot.KindRelease, 0)
		} else if b, e := json.Marshal(rs); e == nil {
			if e := m.snaps.Put(ctx, providers.MusicBrainz, key, snapshot.KindRelease, b, 0); e != nil {
				log.Printf("enrich: caching barcode %s: %v", upc, e)
			}
		}
	}
	return rs, nil
}
