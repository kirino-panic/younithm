package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func newTest(o Options) (*Limiter, *time.Time) {
	now := time.Unix(1000, 0)
	o.Now = func() time.Time { return now }
	o.Exempt = func(r *http.Request) bool { return r.Header.Get("X-Admin") != "" }
	return New(o), &now
}

func do(h http.Handler, remote string, hdr map[string]string) int {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

func TestBurstThenLimitThenRefill(t *testing.T) {
	l, now := newTest(Options{Rate: 1, Burst: 3})
	h := l.Middleware(ok)
	for i := 0; i < 3; i++ {
		if c := do(h, "1.2.3.4:5", nil); c != 200 {
			t.Fatalf("req %d: %d", i, c)
		}
	}
	if c := do(h, "1.2.3.4:5", nil); c != 429 {
		t.Fatalf("want 429, got %d", c)
	}
	if c := do(h, "9.9.9.9:5", nil); c != 200 {
		t.Fatalf("other client limited: %d", c)
	}
	*now = now.Add(2 * time.Second)
	if c := do(h, "1.2.3.4:5", nil); c != 200 {
		t.Fatalf("no refill: %d", c)
	}
}

func TestAdminExempt(t *testing.T) {
	l, _ := newTest(Options{Rate: 1, Burst: 1})
	h := l.Middleware(ok)
	for i := 0; i < 5; i++ {
		if c := do(h, "1.2.3.4:5", map[string]string{"X-Admin": "1"}); c != 200 {
			t.Fatalf("admin limited: %d", c)
		}
	}
}

func TestForwardedForOnlyFromTrustedPeer(t *testing.T) {
	l, _ := newTest(Options{Rate: 1, Burst: 1, Proxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	h := l.Middleware(ok)
	// Untrusted peer: spoofed XFF is ignored, so rotating it doesn't help.
	do(h, "1.2.3.4:5", map[string]string{"X-Forwarded-For": "5.5.5.5"})
	if c := do(h, "1.2.3.4:5", map[string]string{"X-Forwarded-For": "6.6.6.6"}); c != 429 {
		t.Fatalf("spoofed XFF bypassed limit: %d", c)
	}
	// Trusted proxy: distinct real clients get distinct buckets.
	if c := do(h, "10.0.0.1:5", map[string]string{"X-Forwarded-For": "7.7.7.7"}); c != 200 {
		t.Fatalf("%d", c)
	}
	if c := do(h, "10.0.0.1:5", map[string]string{"X-Forwarded-For": "8.8.8.8"}); c != 200 {
		t.Fatalf("%d", c)
	}
	if c := do(h, "10.0.0.1:5", map[string]string{"X-Forwarded-For": "7.7.7.7"}); c != 429 {
		t.Fatalf("%d", c)
	}
}
