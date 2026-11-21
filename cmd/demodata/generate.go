package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// catalog.jsonl holds real Spotify tracks, one per line. aid is the Spotify
// artist the track was picked for ("recent" for the fixed last ten plays);
// recent is that play's rank, 1 = newest.
//
//go:embed catalog.jsonl
var catalogJSONL []byte

type track struct {
	AID        string   `json:"aid"`
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	Album      string   `json:"album"`
	AlbumID    string   `json:"album_id"`
	DurationMs int      `json:"duration_ms"`
	Date       string   `json:"date"`
	Recent     int      `json:"recent"`

	released time.Time
	weight   float64
}

// Share of all plays per artist pool. The five picked artists dominate; the
// "recent" pool is the other artists from the last-ten list, so they show up
// lower in the charts too.
var shares = map[string]float64{
	"4Y345wfGiorcB2NXcsJxOt": 0.25, // t+pazolite
	"3Mws7pLUcCq69go78heq9p": 0.24, // owl*tree
	"5doDHBkL33VHZXcqAuQBj0": 0.22, // Srav3R
	"2UwJRAgSOi1zcLkvUNc8XL": 0.18, // Anamanaguchi
	"7CyG7EIYrSTQNzkV1vvkiH": 0.06, // Kanon Oguni (CHUNITHM / maimai)
	"recent":                 0.05,
}

func parseDate(s string) time.Time {
	for _, f := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func loadCatalog() (pools map[string][]*track, albums map[string][]*track, recents []*track, err error) {
	pools, albums = map[string][]*track{}, map[string][]*track{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(catalogJSONL))
	for sc.Scan() {
		t := &track{}
		if err := json.Unmarshal(sc.Bytes(), t); err != nil {
			return nil, nil, nil, err
		}
		t.released = parseDate(t.Date)
		if t.Recent > 0 {
			recents = append(recents, t)
		}
		key := t.AID + "/" + t.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		pools[t.AID] = append(pools[t.AID], t)
		if t.AID != "recent" {
			albums[t.AlbumID] = append(albums[t.AlbumID], t)
		}
	}
	sort.Slice(recents, func(i, j int) bool { return recents[i].Recent < recents[j].Recent })
	return pools, albums, recents, sc.Err()
}

type listen struct {
	at time.Time
	t  *track
}

// generate writes a ListenBrainz JSONL export covering the two years up to end.
func generate(w io.Writer, end time.Time, seed uint64) (int, error) {
	pools, albums, recents, err := loadCatalog()
	if err != nil {
		return 0, err
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	// Each track gets a fixed popularity (a few favourites, a long tail).
	for _, p := range pools {
		rng.Shuffle(len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
		for i, t := range p {
			t.weight = 1 / math.Pow(float64(i+1), 0.9)
		}
	}
	// Albums long enough for a full run, grouped by artist pool.
	albumsBy := map[string][]string{}
	for id, ts := range albums {
		if len(ts) >= 5 {
			albumsBy[ts[0].AID] = append(albumsBy[ts[0].AID], id)
		}
	}
	for _, ids := range albumsBy {
		sort.Strings(ids)
	}

	// The fixed last ten plays sit at the very end, back to back.
	tail := make([]listen, len(recents))
	at := end.Add(-3 * time.Minute)
	for i, t := range recents { // newest first
		at = at.Add(-time.Duration(t.DurationMs) * time.Millisecond)
		tail[i] = listen{at: at, t: t}
		at = at.Add(-time.Duration(5+rng.IntN(20)) * time.Second)
	}
	cutoff := tail[len(tail)-1].at.Add(-20 * time.Minute)

	keys := []string{"4Y345wfGiorcB2NXcsJxOt", "3Mws7pLUcCq69go78heq9p", "5doDHBkL33VHZXcqAuQBj0", "2UwJRAgSOi1zcLkvUNc8XL", "7CyG7EIYrSTQNzkV1vvkiH", "recent"}
	pool := func() string {
		r := rng.Float64()
		for _, k := range keys {
			if r -= shares[k]; r <= 0 {
				return k
			}
		}
		return "recent"
	}
	pick := func(day time.Time, recency float64) *track {
		// Choose a pool by share, then a track already released by that day.
		aid := pool()
		var total float64
		cands := pools[aid][:0:0]
		for _, t := range pools[aid] {
			if !t.released.After(day) {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return nil
		}
		ws := make([]float64, len(cands))
		for i, t := range cands {
			w := t.weight
			// New releases get a burst in the months after they come out.
			if age := day.Sub(t.released).Hours() / 24; age < 90 {
				w *= 1 + 3*recency*(1-age/90)
			}
			ws[i] = w
			total += w
		}
		r := rng.Float64() * total
		for i, w := range ws {
			if r -= w; r <= 0 {
				return cands[i]
			}
		}
		return cands[len(cands)-1]
	}

	var out []listen
	start := end.AddDate(-2, 0, 0)
	quietUntil := time.Time{}
	for day := start.Truncate(24 * time.Hour); day.Before(end); day = day.AddDate(0, 0, 1) {
		if day.After(quietUntil) && rng.Float64() < 0.012 { // a quiet week now and then
			quietUntil = day.AddDate(0, 0, 4+rng.IntN(6))
		}
		weekend := day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
		sessions := 1 + rng.IntN(2)
		if weekend {
			sessions++
		}
		if !day.After(quietUntil) {
			if rng.Float64() < 0.7 {
				continue
			}
			sessions = 1
		}
		// The library grows over time: early months are lighter.
		growth := 0.55 + 0.45*day.Sub(start).Hours()/end.Sub(start).Hours()
		for s := 0; s < sessions; s++ {
			hour := 18 + rng.IntN(5)
			if weekend || s > 0 {
				hour = 10 + rng.IntN(13)
			}
			at := day.Add(time.Duration(hour)*time.Hour + time.Duration(rng.IntN(3600))*time.Second)
			n := int(float64(6+rng.IntN(18)) * growth)
			var queue []*track
			if ids := albumsBy[pool()]; len(ids) > 0 && rng.Float64() < 0.12 { // play an album through
				for _, t := range albums[ids[rng.IntN(len(ids))]] {
					if !t.released.After(day) {
						queue = append(queue, t)
					}
				}
			}
			for len(queue) < n {
				if t := pick(day, 1); t != nil {
					queue = append(queue, t)
				} else {
					break
				}
			}
			for _, t := range queue {
				if at.After(cutoff) {
					break
				}
				out = append(out, listen{at: at, t: t})
				at = at.Add(time.Duration(t.DurationMs)*time.Millisecond + time.Duration(1+rng.IntN(8))*time.Second)
			}
		}
	}
	for i := len(tail) - 1; i >= 0; i-- {
		out = append(out, tail[i])
	}

	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for _, l := range out {
		rec := map[string]any{
			"listened_at": l.at.Unix(),
			"track_metadata": map[string]any{
				"artist_name":  strings.Join(l.t.Artists, ", "),
				"track_name":   l.t.Title,
				"release_name": l.t.Album,
				"additional_info": map[string]any{
					"spotify_id":        "https://open.spotify.com/track/" + l.t.ID,
					"duration_ms":       l.t.DurationMs,
					"submission_client": "younithm demo data",
				},
			},
		}
		if err := enc.Encode(rec); err != nil {
			return 0, err
		}
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return len(out), nil
}

func init() {
	commands["generate"] = func(args []string) error {
		end := time.Now().UTC().Truncate(time.Second)
		n, err := generate(stdout, end, 2015)
		if err == nil {
			fmt.Fprintf(stderr, "%d listens\n", n)
		}
		return err
	}
}
