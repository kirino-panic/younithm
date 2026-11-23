package ingest

import (
	"sync"
	"time"
)

const (
	nowPlayingDefaultTTL = 5 * time.Minute
	nowPlayingGrace      = 10 * time.Second
)

// NowPlayingTrack is the track part of the now-playing JSON.
type NowPlayingTrack struct {
	Artist        string `json:"artist"`
	Title         string `json:"title"`
	Album         string `json:"album,omitempty"`
	Image         string `json:"image,omitempty"` // cover art URL, when known
	DurationMs    int64  `json:"duration_ms,omitempty"`
	RecordingMBID string `json:"recording_mbid,omitempty"`
	ReleaseMBID   string `json:"release_mbid,omitempty"`
	// The library's track and album, when it already has them.
	RecordingID int64 `json:"recording_id,omitempty"`
	ReleaseID   int64 `json:"release_id,omitempty"`
}

type npEntry struct {
	track     NowPlayingTrack
	source    string
	startedAt time.Time
	expiresAt time.Time
}

// NowPlayingView is the JSON shape of GET /api/v1/now-playing.
type NowPlayingView struct {
	Playing    bool             `json:"playing"`
	Track      *NowPlayingTrack `json:"track,omitempty"`
	Source     string           `json:"source,omitempty"`
	StartedAt  string           `json:"started_at,omitempty"`
	ExpiresAt  string           `json:"expires_at,omitempty"`
	ProgressMs int64            `json:"progress_ms,omitempty"`
}

// NowPlaying is an in-memory, per-user, expiring now-playing store with
// change subscriptions (for SSE).
type NowPlaying struct {
	mu   sync.Mutex
	cur  map[int64]*npEntry
	subs map[chan struct{}]struct{}
	now  func() time.Time
	// Debug: while enabled() is true and nothing is really playing, show a track
	// from pick() (or a made-up one), moving to another when it has run its length.
	enabled func() bool
	pick    func() (NowPlayingTrack, bool)
	fakeCur *npEntry
}

func NewNowPlaying() *NowPlaying {
	return &NowPlaying{cur: map[int64]*npEntry{}, subs: map[chan struct{}]struct{}{}, now: time.Now}
}

// Set makes t the user's current track. It returns when the entry started,
// which SetImage takes to know it is still the same one.
func (n *NowPlaying) Set(user int64, t NowPlayingTrack, source string) time.Time {
	now := n.now()
	ttl := nowPlayingDefaultTTL
	if t.DurationMs > 0 {
		ttl = time.Duration(t.DurationMs)*time.Millisecond + nowPlayingGrace
	}
	n.mu.Lock()
	n.cur[user] = &npEntry{t, source, now, now.Add(ttl)}
	n.mu.Unlock()
	n.notify()
	return now
}

// SetImage gives the entry set at startedAt a cover found later. A newer
// track, or one that already has a cover, is left alone.
func (n *NowPlaying) SetImage(user int64, startedAt time.Time, image string) {
	n.mu.Lock()
	e := n.cur[user]
	hit := e != nil && e.startedAt.Equal(startedAt) && e.track.Image == "" && image != ""
	if hit {
		e.track.Image = image
	}
	n.mu.Unlock()
	if hit {
		n.notify()
	}
}

// ClearIfMatches clears now-playing when the finished listen is the current track.
func (n *NowPlaying) ClearIfMatches(user int64, artist, title string) {
	n.mu.Lock()
	e := n.cur[user]
	hit := e != nil && Normalize(e.track.Artist) == Normalize(artist) && Normalize(e.track.Title) == Normalize(title)
	if hit {
		delete(n.cur, user)
	}
	n.mu.Unlock()
	if hit {
		n.notify()
	}
}

// fallbackTrack is shown by the debug switch when the library has no tracks.
var fallbackTrack = NowPlayingTrack{Artist: "Fake Artist", Title: "Fake Now Playing", Album: "Debug Album", DurationMs: 3*60*1000 + 30*1000}

// fakeEntry returns the current made-up listen, starting a new one when the
// last has finished. Callers hold n.mu.
func (n *NowPlaying) fakeEntry(now time.Time) *npEntry {
	if n.enabled == nil || !n.enabled() {
		n.fakeCur = nil
		return nil
	}
	if n.fakeCur == nil || !now.Before(n.fakeCur.expiresAt) {
		t := fallbackTrack
		if n.pick != nil {
			if p, ok := n.pick(); ok {
				t = p
			}
		}
		if t.DurationMs < 30_000 || t.DurationMs > 10*60_000 {
			t.DurationMs = fallbackTrack.DurationMs
		}
		n.fakeCur = &npEntry{t, "debug", now, now.Add(time.Duration(t.DurationMs) * time.Millisecond)}
	}
	return n.fakeCur
}

func (n *NowPlaying) Get(user int64) NowPlayingView {
	n.mu.Lock()
	defer n.mu.Unlock()
	e := n.cur[user]
	now := n.now()
	if e == nil || !now.Before(e.expiresAt) {
		delete(n.cur, user)
		f := n.fakeEntry(now)
		if f == nil {
			return NowPlayingView{}
		}
		t := f.track
		return NowPlayingView{true, &t, f.source, f.startedAt.UTC().Format(TimeFormat), f.expiresAt.UTC().Format(TimeFormat), now.Sub(f.startedAt).Milliseconds()}
	}
	prog := now.Sub(e.startedAt).Milliseconds()
	if d := e.track.DurationMs; d > 0 && prog > d {
		prog = d
	}
	t := e.track
	return NowPlayingView{true, &t, e.source, e.startedAt.UTC().Format(TimeFormat),
		e.expiresAt.UTC().Format(TimeFormat), prog}
}

// Subscribe returns a channel pinged on every change and a cancel func.
func (n *NowPlaying) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.subs[ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() { n.mu.Lock(); delete(n.subs, ch); n.mu.Unlock() }
}

func (n *NowPlaying) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
