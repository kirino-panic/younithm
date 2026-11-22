package providers

import (
	"sync"
	"time"
)

// Tracklist cache bounds: a batch of listens usually comes from a few dozen
// albums, so each tracklist is fetched once, not once per track.
const (
	tracklistTTL = 30 * time.Minute
	tracklistMax = 256
)

// TracklistCache keeps recent album tracklists, and albums found missing, for
// a ReleaseTracksLookup. The zero value is ready to use.
type TracklistCache struct {
	mu sync.Mutex
	m  map[string]tracklistEntry
}

type tracklistEntry struct {
	tracks []Track
	gone   bool
	at     time.Time
}

// Get returns a cached answer: the tracks, or ErrNotFound for an album found
// missing. ok is false when there is no fresh answer.
func (c *TracklistCache) Get(key string) (tracks []Track, err error, ok bool) {
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if !ok || time.Since(e.at) >= tracklistTTL {
		return nil, nil, false
	}
	if e.gone {
		return nil, ErrNotFound, true
	}
	return append([]Track(nil), e.tracks...), nil, true
}

// Put caches tracks, or with gone set, that the album does not exist.
func (c *TracklistCache) Put(key string, tracks []Track, gone bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= tracklistMax {
		c.m = map[string]tracklistEntry{} // full: drop everything, it is only a cache
	}
	c.m[key] = tracklistEntry{tracks: append([]Track(nil), tracks...), gone: gone, at: time.Now()}
}
