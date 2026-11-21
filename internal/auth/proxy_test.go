package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func mw(trustAll bool, cidrs ...string) *Middleware {
	m := &Middleware{trustProxy: trustAll}
	for _, c := range cidrs {
		m.proxies = append(m.proxies, netip.MustParsePrefix(c))
	}
	return m
}

func req(peer string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "http://app.local/x", nil)
	r.RemoteAddr = peer
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestForwardedHeadersIgnoredWithoutTrust(t *testing.T) {
	m := mw(false)
	r := req("203.0.113.9:5555", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-For": "1.2.3.4",
	})
	if m.secure(r) {
		t.Error("Secure set from untrusted X-Forwarded-Proto")
	}
	if got := m.clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP=%s", got)
	}
}

func TestTrustedProxyCIDR(t *testing.T) {
	m := mw(false, "172.18.0.0/16")
	hdr := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "198.51.100.7"}
	if r := req("172.18.0.3:1", hdr); !m.secure(r) || m.clientIP(r) != "198.51.100.7" {
		t.Error("trusted peer's headers not honored")
	}
	// Peer outside the CIDR: same headers are ignored.
	if r := req("172.19.0.3:1", hdr); m.secure(r) || m.clientIP(r) != "172.19.0.3" {
		t.Error("untrusted peer's headers honored")
	}
}

func TestClientIPWalksChainFromRight(t *testing.T) {
	m := mw(false, "10.0.0.0/8")
	// client-supplied junk on the left, a trusted hop on the right.
	r := req("10.0.0.1:1", map[string]string{"X-Forwarded-For": "9.9.9.9, 198.51.100.7, 10.0.0.2"})
	if got := m.clientIP(r); got != "198.51.100.7" {
		t.Errorf("got %s, spoofed left entry must not win", got)
	}
	r = req("10.0.0.1:1", map[string]string{"X-Forwarded-For": "garbage"})
	if got := m.clientIP(r); got != "10.0.0.1" {
		t.Errorf("malformed XFF: %s", got)
	}
}

func TestTrustAllFlag(t *testing.T) {
	m := mw(true)
	r := req("203.0.113.9:1", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "1.1.1.1, 2.2.2.2"})
	if !m.secure(r) || m.clientIP(r) != "2.2.2.2" {
		t.Error("trust-all behavior changed")
	}
}

func TestSecureFromTLS(t *testing.T) {
	m := mw(false)
	r := req("203.0.113.9:1", nil)
	if m.secure(r) {
		t.Error("plain http marked Secure")
	}
	r.TLS = &tls.ConnectionState{}
	if !m.secure(r) {
		t.Error("TLS not Secure")
	}
}
