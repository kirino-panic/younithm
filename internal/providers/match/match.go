// Package match implements the matching ladder (docs/providers.md):
//
//  1. submitted MBID
//  2. ISRC (strong evidence, not proof: it needs the text or the duration to agree)
//  3. fuzzy artist + title + album + duration with a confidence score
//
// The matcher is pure logic over providers.Provider: it reads from providers
// and returns Links shaped like external_link rows. It never writes to the
// database and never auto-merges conflicts; ambiguity becomes StatusCandidate
// links for the review queue (built later with the import/job system).
// Alias application happens before this package is called (it rewrites the
// Listen), so fixes stick.
package match

import (
	"context"
	"errors"
	"sort"
	"strings"

	"younithm/internal/providers"
)

// exactConfidence is the score at which every compared component is identical
// (allowing for float rounding).
const exactConfidence = 0.999

// Methods and statuses; values equal external_link.match_method / status.
const (
	MethodSubmittedMBID = "submitted_mbid"
	MethodISRC          = "isrc"
	MethodFuzzy         = "fuzzy"
	MethodManual        = "manual" // never produced here
	MethodLookup        = "lookup" // exact lookup of an id we already hold

	StatusAuto      = "auto"
	StatusCandidate = "candidate"
)

// Listen is the matcher's input. Callers (ingest hook, importers) map their
// normalised listen onto it; empty values mean "not provided".
type Listen struct {
	Artist        string
	Title         string
	Album         string
	DurationMs    int
	RecordingMBID string
	// ReleaseIDs are the albums the listen was on, by provider (MusicBrainz
	// release ids, Spotify and Apple Music album ids): ones its history named or
	// a person linked. A candidate on one of them is corroborated; one only on
	// other albums of that provider is not the recording that was played, and
	// the album's tracklist is read to find the one that was. Empty = unknown.
	ReleaseIDs  map[string][]string
	ArtistMBIDs  []string
	ISRC         string
	// ReleaseDate is the release date of the album the listen was on (YYYY,
	// YYYY-MM or YYYY-MM-DD), when known. A candidate recording that is only on
	// releases from other dates is a different edition's recording; one on a
	// release from this date is corroborated. Empty = unknown.
	ReleaseDate string
}

// Link is one proposed recording link (external_link shape).
type Link struct {
	Provider   string
	ProviderID string
	Method     string
	Confidence float64
	Status     string
	// Corroborated says an auto link rests on more than a fuzzy score choosing
	// between names: the listen's own release id singled this recording out, or
	// it was the only candidate and matched exactly. Callers may then treat it as
	// more than a name guess (it is exempt from the "no automatic fuzzy matches"
	// gate).
	Corroborated bool
	// Track is the provider data behind the link, nil for submitted MBIDs
	// (which need no network call).
	Track *providers.Track
}

// Result is everything the ladder found. Links are best first. The review
// queue consumes the links with StatusCandidate.
type Result struct {
	Links []Link
	// Unavailable is true when no provider could be asked at all (none
	// registered, or every one is missing credentials). "Found nothing" is
	// then not a real answer and callers should try again later.
	Unavailable bool
}

// Best returns the top auto link, if any.
func (r Result) Best() (Link, bool) {
	for _, l := range r.Links {
		if l.Status == StatusAuto {
			return l, true
		}
	}
	return Link{}, false
}

// Config tunes thresholds. Zero values take the defaults.
type Config struct {
	AutoThreshold       float64 // fuzzy score needed for auto; default 0.9
	CandidateThreshold  float64 // minimum for a candidate; default 0.6
	AutoMargin          float64 // best must beat runner-up by this; default 0.1
	MaxCandidates       int     // per ladder step; default 5
	DurationToleranceMs int     // full duration credit within; default 3000
	DurationMaxDiffMs   int     // no credit beyond; default 15000
}

func (c Config) withDefaults() Config {
	if c.AutoThreshold == 0 {
		c.AutoThreshold = 0.9
	}
	if c.CandidateThreshold == 0 {
		c.CandidateThreshold = 0.6
	}
	if c.AutoMargin == 0 {
		c.AutoMargin = 0.1
	}
	if c.MaxCandidates == 0 {
		c.MaxCandidates = 5
	}
	if c.DurationToleranceMs == 0 {
		c.DurationToleranceMs = 3000
	}
	if c.DurationMaxDiffMs == 0 {
		c.DurationMaxDiffMs = 15000
	}
	return c
}

// Matcher walks the ladder. Providers are consulted in registry order.
type Matcher struct {
	Providers *providers.Registry
	Config    Config
}

func New(reg *providers.Registry, cfg Config) *Matcher {
	return &Matcher{Providers: reg, Config: cfg.withDefaults()}
}

// Match runs the ladder. It stops at the first rung that yields an auto link
// for the provider; rungs that only produce candidates fall through so a
// later rung may still confirm. A provider error that is a
// *providers.RateLimitedError aborts with that error so the job can retry;
// other per-provider errors are skipped (recorded in the returned error only
// if nothing at all was found).
func (m *Matcher) Match(ctx context.Context, l Listen) (Result, error) {
	cfg := m.Config.withDefaults()
	var res Result
	var firstErr error

	// Rung 1: submitted MBID. Trusted as given; the enrichment job fetches
	// the snapshot separately. MusicBrainz only: a MBID means nothing to
	// other providers.
	if l.RecordingMBID != "" {
		res.Links = append(res.Links, Link{
			Provider: providers.MusicBrainz, ProviderID: l.RecordingMBID,
			Method: MethodSubmittedMBID, Confidence: 1, Status: StatusAuto,
		})
		return res, nil
	}

	provs := m.Providers.All()
	unconfigured := 0
	for _, p := range provs {
		links, err := m.matchProvider(ctx, cfg, p, l)
		if errors.Is(err, providers.ErrNotConfigured) && len(links) == 0 {
			unconfigured++
		}
		var rl *providers.RateLimitedError
		if errors.As(err, &rl) {
			return res, err
		}
		if err != nil && firstErr == nil && !errors.Is(err, providers.ErrUnsupported) &&
			!errors.Is(err, providers.ErrNotConfigured) {
			firstErr = err
		}
		res.Links = append(res.Links, links...)
	}
	res.Unavailable = unconfigured == len(provs)
	sort.SliceStable(res.Links, func(i, j int) bool {
		a, b := res.Links[i], res.Links[j]
		if (a.Status == StatusAuto) != (b.Status == StatusAuto) {
			return a.Status == StatusAuto
		}
		return a.Confidence > b.Confidence
	})
	if len(res.Links) == 0 && firstErr != nil {
		return res, firstErr
	}
	return res, nil
}

func (m *Matcher) matchProvider(ctx context.Context, cfg Config, p providers.Provider, l Listen) ([]Link, error) {
	var firstErr error

	// Rung 2: ISRC.
	var isrcLinks []Link
	if iso, ok := p.(providers.ISRCLookup); ok && l.ISRC != "" {
		tracks, err := iso.LookupISRC(ctx, l.ISRC)
		if err != nil {
			firstErr = err
			if isRateLimited(err) {
				return nil, err
			}
		} else {
			isrcLinks = isrcRung(cfg, l, tracks)
			// A lone ISRC hit is the answer, unless it sits on another album than
			// the one the listen was on (another edition's copy): then the
			// album's tracklist is read as well, and wins if it holds the song.
			if len(isrcLinks) == 1 && isrcLinks[0].Status == StatusAuto && idRelation(l, *isrcLinks[0].Track) != relDiffers {
				return isrcLinks, nil
			}
		}
	}

	// Rung 2b: the album the listen was on. Its tracklist is a closed set that
	// holds the recording that was played, so the matcher reads the track off it
	// instead of searching by name. A single scorable track on that album is an
	// id-backed match (see fuzzyRung); several leave candidates.
	var pool []providers.Track
	if rl, ok := p.(providers.ReleaseTracksLookup); ok && len(l.ReleaseIDs[p.Name()]) > 0 && l.Title != "" && l.Artist != "" {
		seen := map[string]bool{}
		for _, id := range l.ReleaseIDs[p.Name()] {
			if len(seen) == maxReleaseLookups {
				break
			}
			if seen[strings.ToLower(id)] {
				continue
			}
			seen[strings.ToLower(id)] = true
			tracks, err := rl.LookupReleaseTracks(ctx, id)
			if err != nil {
				if errors.Is(err, providers.ErrNotFound) {
					continue // a dead release id says nothing: search by name instead
				}
				if firstErr == nil {
					firstErr = err
				}
				if isRateLimited(err) {
					return isrcLinks, err
				}
				break
			}
			pool = mergeTracks(pool, tracks)
		}
	}

	// Rung 3: fuzzy. The search gets easier step by step, and only while it has
	// found nothing worth a candidate: a query that returns something scorable
	// stops the ladder, so a track that is found costs one request.
	var fuzzy []Link
	if l.Title != "" && l.Artist != "" {
		fuzzy = fuzzyRung(cfg, l, pool)
		if len(fuzzy) == 0 {
			for _, step := range searchSteps(p, l) {
				tracks, err := step(ctx)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					if isRateLimited(err) {
						return isrcLinks, err
					}
					break
				}
				pool = mergeTracks(pool, tracks)
				if fuzzy = fuzzyRung(cfg, l, pool); len(fuzzy) > 0 {
					break
				}
			}
		}
	}

	// Merge, de-duplicating by provider id (ISRC evidence wins on ties).
	out := append([]Link(nil), isrcLinks...)
	for _, f := range fuzzy {
		dup := false
		for i := range out {
			if out[i].ProviderID == f.ProviderID {
				dup = true
				// Fuzzy corroboration of an ISRC candidate: keep the higher confidence.
				if f.Confidence > out[i].Confidence {
					out[i].Confidence = f.Confidence
				}
			}
		}
		if !dup {
			out = append(out, f)
		}
	}
	// The album's own track beats an ISRC hit on another edition.
	for _, f := range fuzzy {
		if f.Status == StatusAuto && f.Track != nil && idRelation(l, *f.Track) == relAgrees {
			for i := range out {
				if out[i].Method == MethodISRC && out[i].Status == StatusAuto && out[i].ProviderID != f.ProviderID {
					out[i].Status = StatusCandidate
				}
			}
			break
		}
	}
	// If ISRC and fuzzy agree on one id, it may now deserve auto.
	if len(out) > 0 && len(isrcLinks) > 0 && len(fuzzy) > 0 &&
		out[0].Status == StatusCandidate && out[0].Method == MethodISRC &&
		out[0].ProviderID == fuzzy[0].ProviderID && fuzzy[0].Confidence >= cfg.AutoThreshold {
		out[0].Status = StatusAuto
	}
	if len(out) > cfg.MaxCandidates {
		out = out[:cfg.MaxCandidates]
	}
	return out, firstErr
}

// maxReleaseLookups caps how many of the listen's releases are read per
// listen (a history rarely names more than one).
const maxReleaseLookups = 3

// searchSteps lists the searches for a listen, strictest first: the exact
// phrases of title, artist and album, then the title without its credit tag
// and without the album, then (for providers that offer LooseSearch) word
// searches that do not depend on one field matching letter for letter.
func searchSteps(p providers.Provider, l Listen) []func(context.Context) ([]providers.Track, error) {
	title := SearchTitle(l.Title)
	var steps []func(context.Context) ([]providers.Track, error)
	search := func(q providers.TrackQuery) {
		steps = append(steps, func(ctx context.Context) ([]providers.Track, error) { return p.SearchTrack(ctx, q) })
	}
	search(providers.TrackQuery{Artist: l.Artist, Title: l.Title, Album: l.Album, DurationMs: l.DurationMs, Limit: 10})
	if title != l.Title || l.Album != "" {
		search(providers.TrackQuery{Artist: l.Artist, Title: title, DurationMs: l.DurationMs, Limit: 10})
	}
	if ls, ok := p.(providers.LooseSearch); ok {
		loose := func(q providers.TrackQuery) {
			q.DurationMs, q.Limit = l.DurationMs, 25
			steps = append(steps, func(ctx context.Context) ([]providers.Track, error) { return ls.SearchLoose(ctx, q) })
		}
		if l.Album != "" {
			loose(providers.TrackQuery{Artist: l.Artist, Album: l.Album}) // the album's tracks by this artist
			loose(providers.TrackQuery{Title: title, Album: l.Album})     // credits that differ
		}
		loose(providers.TrackQuery{Title: title, Artist: l.Artist}) // an album that differs
	}
	return steps
}

// mergeTracks adds tracks to a pool, once per provider id: a recording found
// twice keeps every release either answer listed.
func mergeTracks(pool, add []providers.Track) []providers.Track {
	at := map[string]int{}
	for i, t := range pool {
		at[t.Provider+"\x00"+strings.ToLower(t.ProviderID)] = i
	}
	for _, t := range add {
		k := t.Provider + "\x00" + strings.ToLower(t.ProviderID)
		i, ok := at[k]
		if !ok {
			at[k] = len(pool)
			pool = append(pool, t)
			continue
		}
		have := pool[i].Releases
	next:
		for _, r := range t.Releases {
			for _, h := range have {
				if strings.EqualFold(h.ProviderID, r.ProviderID) {
					continue next
				}
			}
			pool[i].Releases = append(pool[i].Releases, r)
		}
		if len(pool[i].ISRCs) == 0 {
			pool[i].ISRCs = t.ISRCs
		}
		if pool[i].DurationMs == 0 {
			pool[i].DurationMs = t.DurationMs
		}
	}
	return pool
}

func isRateLimited(err error) bool {
	var rl *providers.RateLimitedError
	return errors.As(err, &rl)
}

// score returns a 0..1 confidence comparing a provider track with the listen,
// as a weighted mean over the components that are available on both sides.
// Title and artist are always present.
func score(cfg Config, l Listen, t providers.Track) (conf, titleSim, artistSim float64) {
	titleSim = TitleSimilarity(l.Title, t.Title)

	la := NormArtist(l.Artist)
	for _, a := range t.Artists {
		if s := Similarity(la, NormArtist(a.Name)); s > artistSim {
			artistSim = s
		}
	}
	// Joined credit ("A & B") vs a single submitted string.
	if len(t.Artists) > 1 {
		var joined string
		for i, a := range t.Artists {
			if i > 0 {
				joined += " "
			}
			joined += a.Name
		}
		if s := Similarity(la, NormArtist(joined)); s > artistSim {
			artistSim = s
		}
	}

	sum, wsum := 0.45*titleSim+0.35*artistSim, 0.8
	if l.Album != "" && len(t.Releases) > 0 {
		best := 0.0
		na := NormTitle(l.Album)
		for _, r := range t.Releases {
			if s := Similarity(na, NormTitle(r.Title)); s > best {
				best = s
			}
		}
		sum += 0.1 * best
		wsum += 0.1
	}
	if l.DurationMs > 0 && t.DurationMs > 0 {
		d := l.DurationMs - t.DurationMs
		if d < 0 {
			d = -d
		}
		var ds float64
		switch {
		case d <= cfg.DurationToleranceMs:
			ds = 1
		case d >= cfg.DurationMaxDiffMs:
			ds = 0
		default:
			ds = 1 - float64(d-cfg.DurationToleranceMs)/float64(cfg.DurationMaxDiffMs-cfg.DurationToleranceMs)
		}
		sum += 0.1 * ds
		wsum += 0.1
	}
	return sum / wsum, titleSim, artistSim
}

// durationAgrees reports whether both durations are known and within the
// tolerance. Unlike a title or an artist name, a duration reads the same in
// every language and script.
func durationAgrees(cfg Config, a, b int) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= cfg.DurationToleranceMs
}

// isrcRung turns ISRC hits into links. One hit is auto when something besides
// the ISRC corroborates it: the text looks like the listen, or the durations
// agree (so a Japanese title against its romanisation, or credits that differ
// between platforms, do not demote the same recording). A hit with neither is a
// low-confidence candidate, and several hits are always candidates, because
// ISRCs can be shared across recordings and reissues.
func isrcRung(cfg Config, l Listen, tracks []providers.Track) []Link {
	var links []Link
	for i := range tracks {
		t := tracks[i]
		conf, ts, as := score(cfg, l, t)
		plausible := l.Title == "" || (ts >= 0.7 && as >= 0.7)
		corroborated := durationAgrees(cfg, l.DurationMs, t.DurationMs) || listenRelation(l, t) == relAgrees
		if !plausible && !corroborated && l.Title != "" && l.Artist != "" {
			// ISRC says yes, nothing else does: low-confidence candidate.
			links = append(links, Link{Provider: t.Provider, ProviderID: t.ProviderID, Method: MethodISRC, Confidence: 0.4, Status: StatusCandidate, Track: &tracks[i]})
			continue
		}
		c := 0.9
		if plausible && l.Title != "" {
			c = 0.8 + 0.2*conf // 0.8..1.0
		}
		links = append(links, Link{Provider: t.Provider, ProviderID: t.ProviderID, Method: MethodISRC, Confidence: c, Status: StatusAuto, Track: &tracks[i]})
	}
	sort.SliceStable(links, func(i, j int) bool { return links[i].Confidence > links[j].Confidence })
	if len(links) > 1 {
		// Several recordings share the ISRC: ambiguous, never auto, unless the
		// album singles one out, by the release id the listen named or by the
		// release date: exactly one is on that release (or on a release from that
		// date) and every other one only on other releases.
		only := -1
		for i := range links {
			switch listenRelation(l, *links[i].Track) {
			case relAgrees:
				if only == -1 {
					only = i
				} else {
					only = -2 // a second one agrees: still ambiguous
				}
			case relDiffers:
			default:
				only = -2 // an unknown one could be the right one
			}
		}
		for i := range links {
			links[i].Status = StatusCandidate
			if i == only && links[i].Confidence > 0.4 {
				links[i].Status = StatusAuto
			}
		}
	}
	if len(links) > cfg.MaxCandidates {
		links = links[:cfg.MaxCandidates]
	}
	return links
}

func fuzzyRung(cfg Config, l Listen, tracks []providers.Track) []Link {
	type scored struct {
		i    int
		conf float64
		ok   bool // title and artist both strong
	}
	var sc []scored
	for i, t := range tracks {
		c, ts, as := score(cfg, l, t)
		if c < cfg.CandidateThreshold {
			continue
		}
		sc = append(sc, scored{i, c, ts >= 0.85 && as >= 0.85})
	}
	sort.SliceStable(sc, func(a, b int) bool { return sc[a].conf > sc[b].conf })
	// The runner-up that counts against the best candidate. When the best one is
	// on a release from the listen's date, a runner-up that is only on releases
	// from other dates is another edition's recording (MusicBrainz often keeps
	// one per edition), not a rival, so it is skipped here; it stays a candidate.
	runnerUp := -1.0
	if len(sc) > 1 {
		topRel := releaseRelation(l.ReleaseDate, tracks[sc[0].i])
		for _, s := range sc[1:] {
			if topRel == relAgrees && releaseRelation(l.ReleaseDate, tracks[s.i]) == relDiffers {
				continue
			}
			runnerUp = s.conf
			break
		}
	}
	// The release the listen named singles out one recording: of the candidates
	// (several MusicBrainz recordings of one song are normal, one per master)
	// exactly one is on that release. That is an id, not a guess, so it is
	// auto whatever the margin to the others, as long as its title and artist
	// read as the listen's. Two on the release is still ambiguous.
	byRelease := false
	if n, at := 0, -1; len(l.ReleaseIDs) > 0 {
		for j, s := range sc {
			if idRelation(l, tracks[s.i]) == relAgrees {
				n++
				at = j
			}
		}
		if n == 1 && sc[at].ok {
			byRelease = true
			sc[0], sc[at] = sc[at], sc[0]
			runnerUp = -1
		}
	}
	// A lone candidate that reads exactly like the listen (title and artist, and
	// the album and duration where both sides have them, all identical once
	// normalised) has no rival and nothing to disagree with: it is not a guess
	// between names, so it too is exempt from the fuzzy-consent gate.
	sole := len(sc) == 1 && sc[0].ok && sc[0].conf >= exactConfidence
	var links []Link
	for rank, s := range sc {
		t := tracks[s.i]
		status := StatusCandidate
		corroborated := false
		switch {
		case rank == 0 && byRelease:
			status, corroborated = StatusAuto, true
		case rank == 0 && s.ok && s.conf >= cfg.AutoThreshold &&
			(runnerUp < 0 || s.conf-runnerUp >= cfg.AutoMargin):
			status, corroborated = StatusAuto, sole
		}
		links = append(links, Link{Provider: t.Provider, ProviderID: t.ProviderID, Method: MethodFuzzy, Confidence: s.conf, Status: status, Corroborated: corroborated, Track: &tracks[s.i]})
		if len(links) == cfg.MaxCandidates {
			break
		}
	}
	return links
}
