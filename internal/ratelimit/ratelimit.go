// Package ratelimit provides a per-client token-bucket limiter for the public
// read endpoints. Admin sessions are exempt.
package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"younithm/internal/auth"
)

// Options configures a Limiter.
type Options struct {
	Rate     float64        // tokens per second (default 2)
	Burst    int            // bucket size (default 30)
	Proxies  []netip.Prefix // peers whose X-Forwarded-For is believed
	TrustAll bool           // believe X-Forwarded-For from any peer
	Now      func() time.Time
	Exempt   func(*http.Request) bool // default: auth.IsAdminSession
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is safe for concurrent use.
type Limiter struct {
	o       Options
	mu      sync.Mutex
	buckets map[string]*bucket
	sweep   time.Time
}

// New returns a Limiter with defaults filled in.
func New(o Options) *Limiter {
	if o.Rate <= 0 {
		o.Rate = 2
	}
	if o.Burst <= 0 {
		o.Burst = 30
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Exempt == nil {
		o.Exempt = auth.IsAdminSession
	}
	return &Limiter{o: o, buckets: map[string]*bucket{}}
}

// Middleware answers 429 with Retry-After when the client's bucket is empty.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.o.Exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		if ok, wait := l.allow(l.clientKey(r)); !ok {
			secs := int(wait.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limited","message":"Too many requests."}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) allow(key string) (bool, time.Duration) {
	now := l.o.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.sweep) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.sweep = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(l.o.Burst), last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.o.Rate
	if b.tokens > float64(l.o.Burst) {
		b.tokens = float64(l.o.Burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.o.Rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// clientKey is the peer IP, or the right-most untrusted X-Forwarded-For entry
// when the peer is a trusted proxy.
func (l *Limiter) clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !l.trusted(peer) {
		return peer.String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !l.trusted(a) {
			return a.String()
		}
	}
	return peer.String()
}

func (l *Limiter) trusted(a netip.Addr) bool {
	if l.o.TrustAll {
		return true
	}
	for _, p := range l.o.Proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
