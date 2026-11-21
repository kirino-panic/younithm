package match

import (
	"strings"

	"younithm/internal/providers"
)

// relation says how a candidate recording's releases compare with the album
// the listen was on, by release date.
type relation int

const (
	relUnknown relation = iota // no date on one side: says nothing
	relAgrees                  // on a release from the listen's date
	relDiffers                 // only on releases from other dates
)

// DatesAgree is datesAgree for other packages.
func DatesAgree(a, b string) (agree, ok bool) { return datesAgree(a, b) }

// datesAgree compares two release dates (YYYY, YYYY-MM or YYYY-MM-DD) at the
// coarser of their precisions, so "2025" agrees with "2025-11-14". ok is false
// when either is missing or too short to compare.
func datesAgree(a, b string) (agree, ok bool) {
	n := min(len(a), len(b))
	if n < 4 {
		return false, false
	}
	return a[:n] == b[:n], true
}

// releaseRelation compares the listen's album date with the dates of the
// releases t is on. A date is the same in every language and script, and the
// lookups that produced t already list its releases, so this costs no request.
func releaseRelation(date string, t providers.Track) relation {
	if date == "" {
		return relUnknown
	}
	known := false
	for _, r := range t.Releases {
		agree, ok := datesAgree(date, r.Date)
		if !ok {
			continue
		}
		known = true
		if agree {
			return relAgrees
		}
	}
	if known {
		return relDiffers
	}
	return relUnknown
}

// idRelation compares the MusicBrainz releases the listen named with the
// releases a candidate MusicBrainz recording is on. Release ids are
// provider-specific, so only MusicBrainz candidates are judged, and one that
// lists no releases says nothing.
func idRelation(l Listen, t providers.Track) relation {
	if len(l.ReleaseMBIDs) == 0 || t.Provider != providers.MusicBrainz || len(t.Releases) == 0 {
		return relUnknown
	}
	for _, r := range t.Releases {
		for _, id := range l.ReleaseMBIDs {
			if r.ProviderID != "" && strings.EqualFold(r.ProviderID, id) {
				return relAgrees
			}
		}
	}
	return relDiffers
}

// listenRelation is how a candidate relates to the album the listen was on:
// by the release id the listen named when that says anything, else by date.
func listenRelation(l Listen, t providers.Track) relation {
	if rel := idRelation(l, t); rel != relUnknown {
		return rel
	}
	return releaseRelation(l.ReleaseDate, t)
}
